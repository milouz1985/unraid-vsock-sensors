// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"gopkg.in/ini.v1"
)

const (
	defaultDisksINIPath       = "/var/local/emhttp/disks.ini"
	defaultDevsINIPath        = "/var/local/emhttp/devs.ini"
	defaultSMARTCacheDir      = "/var/local/emhttp/smart"
	defaultUnraidVarINIPath   = "/var/local/emhttp/var.ini"
	defaultPollAttributes     = 30 * time.Second
	diskWatchdogInterval      = 5 * time.Second
	diskSnapshotTimeout       = 3 * diskWatchdogInterval
	diskFailureMargin         = 5 * time.Second
	emhttpPollMargin          = 15 * time.Second
	minimumSMARTFreshness     = 10 * time.Second
	maximumRecommendedPolling = 60 * time.Second
	// Tests on Unraid suggest disk IDs in disks.ini and devs.ini are truncated
	// to 79 bytes. This is an observed limit, not a documented guarantee.
	maxUnraidDiskIDSize = 79
)

type diskDataPaths struct {
	disksINI     string
	devsINI      string
	smartDir     string
	varINI       string
	sysBlockRoot string
	policyFile   string
	sdspin       string
	smartctlType string
}

var defaultDiskDataPaths = diskDataPaths{
	disksINI: defaultDisksINIPath, devsINI: defaultDevsINIPath,
	smartDir: defaultSMARTCacheDir, varINI: defaultUnraidVarINIPath,
	sysBlockRoot: defaultSysBlockRoot, policyFile: defaultDiskPolicyFile,
	sdspin: defaultSDSpinPath, smartctlType: defaultSmartctlTypePath,
}

type diskCollector struct {
	paths    diskDataPaths
	watchdog time.Duration
	now      func() time.Time

	refreshMu      sync.Mutex
	mu             sync.RWMutex
	readings       []sensors.Disk
	err            error
	updatedAt      time.Time
	state          diskStateTracker
	policyLog      stickyErrorLog
	lastEmhttpPoll atomic.Pointer[time.Time]
	fallbackActive bool
	fallbackLog    stickyErrorLog

	pollLogInitialized bool
	lastPollInterval   time.Duration
	lastPollError      string
}

type diskState struct {
	lastValid   float64
	hasValid    bool
	failedSince time.Time
	hardFailed  bool
}

type diskStateTracker map[string]diskState

// unraidDisk is inventory, power state and cached temperature produced by
// Unraid. smartName locates the matching report in /var/local/emhttp/smart.
type unraidDisk struct {
	id, name, device, transport, temperature, smartName string
	rotational                                          bool
	spundown                                            bool
}

type diskObservation struct {
	disk        unraidDisk
	temperature float64
	standby     bool
	err         error
	// noGrace prevents an old emhttpd temperature from hiding a failed
	// direct read after the emhttpd polling heartbeat has expired.
	noGrace bool
}

type diskInventoryEntry struct {
	disk     unraidDisk
	bus      diskBus
	policy   diskPolicy
	included bool
}

func newDiskCollector(paths diskDataPaths) *diskCollector {
	return &diskCollector{
		paths: paths, watchdog: diskWatchdogInterval, now: time.Now,
		err:         errors.New("disk temperatures have not been collected yet"),
		state:       make(diskStateTracker),
		policyLog:   stickyErrorLog{context: "disk policies"},
		fallbackLog: stickyErrorLog{context: "direct SMART fallback"},
	}
}

func (c *diskCollector) run(ctx context.Context, refresh <-chan struct{}) {
	runDiskRefreshLoop(ctx, refresh, c.watchdog, func() { c.refreshWithContext(ctx) })
}

func (c *diskCollector) noteEmhttpPoll() {
	now := c.now()
	c.lastEmhttpPoll.Store(&now)
}

func runDiskRefreshLoop(ctx context.Context, refresh <-chan struct{}, watchdog time.Duration, collect func()) {
	collect()
	ticker := time.NewTicker(watchdog)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh:
			collect()
		case <-ticker.C:
			collect()
		}
	}
}

func requestDiskRefresh(refresh chan<- struct{}) {
	select {
	case refresh <- struct{}{}:
	default:
	}
}

func (c *diskCollector) refresh() {
	c.refreshWithContext(context.Background())
}

func (c *diskCollector) refreshWithContext(ctx context.Context) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	now := c.now()
	pollInterval, configErr := readPollAttributes(c.paths.varINI)
	c.logPollAttributesChange(pollInterval, configErr)
	freshness := smartFreshnessWindow(pollInterval)
	lastPoll := c.lastEmhttpPoll.Load()
	if lastPoll == nil {
		c.lastEmhttpPoll.CompareAndSwap(nil, &now)
		lastPoll = c.lastEmhttpPoll.Load()
	}
	fallback := pollInterval > 0 && now.Sub(*lastPoll) > pollInterval+emhttpPollMargin
	if fallback != c.fallbackActive {
		if fallback {
			log.Printf("emhttpd SMART polling stale, enabling direct SMART fallback")
		} else {
			log.Printf("emhttpd SMART polling recovered, disabling direct SMART fallback")
		}
		c.fallbackActive = fallback
		if !fallback {
			c.fallbackLog.update(nil)
		}
	}

	policyFile := c.paths.policyFile
	if policyFile == "" {
		policyFile = defaultDiskPolicyFile
	}
	policies, policyErr := readDiskPolicies(policyFile)
	c.policyLog.update(policyErr)
	selector := &diskSelector{sysBlockRoot: c.paths.sysBlockRoot, policies: policies}
	disks, err := readDiskInventory(c.paths.disksINI, c.paths.devsINI, selector)
	var readings []sensors.Disk
	if err != nil {
		c.state.markFailure(now)
	} else {
		var observations []diskObservation
		if fallback {
			observations = c.collectFallback(ctx, disks)
		} else {
			observations = makeDiskObservations(disks, c.paths.smartDir, now, freshness)
		}
		readings = c.state.apply(observations, now, pollInterval+diskFailureMargin)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		c.updatedAt = time.Time{}
		return
	}
	c.readings = readings
	c.updatedAt = now
}

func (c *diskCollector) snapshot() ([]sensors.Disk, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.err != nil {
		return nil, c.err
	}
	if !c.now().Before(c.updatedAt.Add(diskSnapshotTimeout)) {
		return nil, errors.New("disk cache snapshot expired")
	}
	return slices.Clone(c.readings), nil
}

func smartFreshnessWindow(pollInterval time.Duration) time.Duration {
	margin := pollInterval / 5
	if margin < minimumSMARTFreshness {
		margin = minimumSMARTFreshness
	}
	return pollInterval + margin
}

func parsePollAttributes(data []byte) (time.Duration, error) {
	config, err := ini.Load(data)
	if err != nil {
		return 0, fmt.Errorf("parse var.ini: %w", err)
	}
	section := config.Section(ini.DefaultSection)
	key, err := section.GetKey("poll_attributes")
	if err != nil {
		return 0, errors.New("poll_attributes is missing")
	}
	raw := strings.TrimSpace(key.String())
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("poll_attributes %q is not a number", raw)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("poll_attributes %d is negative", seconds)
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("poll_attributes %d is too large", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

func readPollAttributes(path string) (time.Duration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultPollAttributes, fmt.Errorf("read %s: %w", path, err)
	}
	interval, err := parsePollAttributes(data)
	if err != nil {
		return defaultPollAttributes, fmt.Errorf("read %s: %w", path, err)
	}
	return interval, nil
}

func (c *diskCollector) logPollAttributesChange(interval time.Duration, configErr error) {
	errorMessage := ""
	if configErr != nil {
		errorMessage = configErr.Error()
	}
	if c.pollLogInitialized && c.lastPollInterval == interval && c.lastPollError == errorMessage {
		return
	}
	c.pollLogInitialized = true
	c.lastPollInterval = interval
	c.lastPollError = errorMessage
	logPollAttributes(interval, configErr)
}

func logPollAttributes(interval time.Duration, configErr error) {
	if configErr != nil {
		log.Printf("warning: %v; using %s for SMART cache freshness and stalled-poll detection", configErr, defaultPollAttributes)
		return
	}
	if interval == 0 {
		log.Printf("warning: Unraid automatic SMART polling is disabled (poll_attributes=0); disk temperatures cannot remain fresh automatically")
		return
	}
	log.Printf("Unraid SMART polling interval: %s", interval)
	if interval > maximumRecommendedPolling {
		log.Printf("warning: Unraid SMART polling interval is %s; fan control may react with several minutes of delay", interval)
	}
}

func readDiskInventory(disksINIPath, devsINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readDiskInventoryEntries(disksINIPath, devsINIPath, selector, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
}

func readDiskInventoryEntries(disksINIPath, devsINIPath string, selector *diskSelector, requireValid bool) ([]diskInventoryEntry, error) {
	assigned, err := readAssignedEntries(disksINIPath, selector, requireValid)
	if err != nil {
		return nil, err
	}
	flashIDs := make(map[string]struct{})
	flashDevices := make(map[string]struct{})
	for _, entry := range assigned {
		if strings.EqualFold(entry.disk.name, "flash") {
			if entry.disk.id != "" {
				flashIDs[entry.disk.id] = struct{}{}
			}
			if entry.disk.device != "" {
				flashDevices[entry.disk.device] = struct{}{}
			}
		}
	}
	unassigned, err := readUnassignedEntries(devsINIPath, selector, flashIDs, flashDevices, requireValid)
	if err != nil {
		return nil, err
	}

	merged := make([]diskInventoryEntry, 0, len(assigned)+len(unassigned))
	seenMergedIDs := make(map[string]struct{}, len(assigned)+len(unassigned))
	for _, inventory := range [][]diskInventoryEntry{assigned, unassigned} {
		for _, entry := range inventory {
			if _, duplicate := seenMergedIDs[entry.disk.id]; duplicate && entry.disk.id != "" {
				continue
			}
			seenMergedIDs[entry.disk.id] = struct{}{}
			merged = append(merged, entry)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].disk.name == merged[j].disk.name {
			return merged[i].disk.id < merged[j].disk.id
		}
		return merged[i].disk.name < merged[j].disk.name
	})
	return merged, nil
}

func includedDisks(entries []diskInventoryEntry) []unraidDisk {
	var disks []unraidDisk
	for _, entry := range entries {
		if entry.included {
			disks = append(disks, entry.disk)
		}
	}
	return disks
}

// readDisks reads assigned-disk state from Unraid. The logical section name
// selects the SMART cache report (for example disk1 -> smart/disk1).
func readDisks(disksINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readAssignedEntries(disksINIPath, selector, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
}

func readAssignedEntries(disksINIPath string, selector *diskSelector, requireValid bool) ([]diskInventoryEntry, error) {
	config, err := ini.Load(disksINIPath)
	if err != nil {
		return nil, err
	}

	var entries []diskInventoryEntry
	seenIDs := make(map[string]struct{})
	sections := 0
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		sections++
		name := strings.Trim(section.Name(), "\"")
		transport := diskTransport(section)
		id := strings.TrimSpace(section.Key("id").String())
		device := normalizeDiskDevice(section.Key("device").String())
		status := strings.ToUpper(strings.TrimSpace(section.Key("status").String()))
		// Unraid uses the _NP marker for states without a physical disk. Keep
		// every other state so degraded, disabled and emulated disks remain.
		if strings.Contains(status, "_NP") {
			continue
		}
		if id != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				return nil, fmt.Errorf("duplicate disk ID %q in disks.ini", id)
			}
			seenIDs[id] = struct{}{}
		}
		bus, policy, included := selector.evaluate(id, name, device)
		entry := diskInventoryEntry{
			disk: diskFromSection(section, id, name, device, name, transport),
			bus:  bus, policy: policy, included: included,
		}
		if !included || !requireValid {
			entries = append(entries, entry)
			continue
		}
		if id == "" {
			return nil, fmt.Errorf("active disk %q has no stable ID", name)
		}
		if len(id) > maxUnraidDiskIDSize {
			return nil, fmt.Errorf("active disk %q has a %d-byte stable ID; observed emhttpd limit is %d", name, len(id), maxUnraidDiskIDSize)
		}
		if device == "" {
			return nil, fmt.Errorf("active disk %q has no device", name)
		}
		entries = append(entries, entry)
	}
	if sections == 0 {
		return nil, errors.New("disk inventory contains no sections")
	}
	return entries, nil
}

// readUnassignedDisks reads Unraid's native unassigned-device inventory. The
// device name only selects the SMART report; the sensor identity remains id.
func readUnassignedDisks(devsINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readUnassignedEntries(devsINIPath, selector, nil, nil, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
}

func readUnassignedEntries(devsINIPath string, selector *diskSelector, flashIDs, flashDevices map[string]struct{}, requireValid bool) ([]diskInventoryEntry, error) {
	config, err := ini.Load(devsINIPath)
	if err != nil {
		return nil, err
	}

	var entries []diskInventoryEntry
	seenIDs := make(map[string]struct{})
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")
		transport := diskTransport(section)
		id := strings.TrimSpace(section.Key("id").String())
		device := normalizeDiskDevice(section.Key("device").String())
		if id != "" && device != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				return nil, fmt.Errorf("duplicate disk ID %q in devs.ini", id)
			}
			seenIDs[id] = struct{}{}
		}
		if _, flash := flashIDs[id]; flash {
			continue
		}
		if _, flash := flashDevices[device]; flash {
			continue
		}
		bus, policy, included := selector.evaluate(id, name, device)
		entry := diskInventoryEntry{
			disk: diskFromSection(section, id, name, device, device, transport),
			bus:  bus, policy: policy, included: included,
		}
		if !included || !requireValid {
			entries = append(entries, entry)
			continue
		}
		if device == "" {
			continue
		}
		if id == "" {
			return nil, fmt.Errorf("unassigned disk %q has no stable ID", name)
		}
		if len(id) > maxUnraidDiskIDSize {
			return nil, fmt.Errorf("unassigned disk %q has a %d-byte stable ID; observed emhttpd limit is %d", name, len(id), maxUnraidDiskIDSize)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func diskTransport(section *ini.Section) string {
	return strings.ToLower(strings.TrimSpace(section.Key("transport").String()))
}

func diskFromSection(section *ini.Section, id, name, device, smartName, transport string) unraidDisk {
	return unraidDisk{
		id: id, name: name, device: device, smartName: smartName,
		transport:   transport,
		temperature: strings.TrimSpace(section.Key("temp").String()),
		rotational:  strings.TrimSpace(section.Key("rotational").String()) == "1",
		spundown:    strings.TrimSpace(section.Key("spundown").String()) == "1",
	}
}

func normalizeDiskDevice(device string) string {
	device = strings.TrimSpace(device)
	if strings.HasPrefix(device, "/dev/") {
		device = strings.TrimPrefix(device, "/dev/")
	}
	if device == "." || device == ".." || strings.ContainsRune(device, filepath.Separator) {
		return ""
	}
	return device
}

func makeDiskObservations(disks []unraidDisk, smartDir string, now time.Time, freshness time.Duration) []diskObservation {
	observations := make([]diskObservation, 0, len(disks))
	for _, disk := range disks {
		observation := diskObservation{disk: disk, standby: disk.spundown}
		if disk.spundown {
			observations = append(observations, observation)
			continue
		}

		observation.temperature, observation.err = parseCachedTemperature(disk.temperature)
		if observation.err == nil {
			info, err := os.Stat(filepath.Join(smartDir, disk.smartName))
			if err != nil {
				observation.err = fmt.Errorf("SMART cache for %s: %w", disk.name, err)
			} else if now.Sub(info.ModTime()) > freshness {
				observation.err = fmt.Errorf("SMART cache for %s is stale by %s", disk.name, now.Sub(info.ModTime())-freshness)
			}
		}
		observations = append(observations, observation)
	}
	return observations
}

func parseCachedTemperature(raw string) (float64, error) {
	if raw == "" || raw == "*" {
		return 0, errors.New("cached temperature is unavailable")
	}
	temperature, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("cached temperature %q is not numeric", raw)
	}
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) {
		return 0, fmt.Errorf("cached temperature %q is invalid", raw)
	}
	return temperature, nil
}

func (s diskStateTracker) markFailure(now time.Time) {
	for id, state := range s {
		if state.failedSince.IsZero() {
			state.failedSince = now
			s[id] = state
		}
	}
}

func (s diskStateTracker) apply(observations []diskObservation, now time.Time, grace time.Duration) []sensors.Disk {
	present := make(map[string]struct{}, len(observations))
	readings := make([]sensors.Disk, 0, len(observations))
	for _, observation := range observations {
		disk := observation.disk
		present[disk.id] = struct{}{}
		state := s[disk.id]
		temperature, unavailable := observation.temperature, false
		switch {
		case observation.standby:
			temperature = 0
			state.failedSince = time.Time{}
			state.hardFailed = false
		case observation.err == nil:
			state.lastValid = observation.temperature
			state.hasValid = true
			state.failedSince = time.Time{}
			state.hardFailed = false
		default:
			if state.failedSince.IsZero() {
				state.failedSince = now
			}
			state.hardFailed = state.hardFailed || observation.noGrace
			if !state.hardFailed && state.hasValid && now.Sub(state.failedSince) < grace {
				temperature = state.lastValid
			} else {
				temperature = 0
				unavailable = true
			}
		}
		s[disk.id] = state
		readings = append(readings, sensors.Disk{
			ID: disk.id, Name: disk.name, Device: disk.device,
			Transport: disk.transport, Rotational: disk.rotational,
			Temp: temperature, Unavailable: unavailable,
		})
	}
	for id := range s {
		if _, ok := present[id]; !ok {
			delete(s, id)
		}
	}
	return readings
}
