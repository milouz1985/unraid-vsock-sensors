// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

const (
	defaultDisksINIPath       = "/var/local/emhttp/disks.ini"
	defaultDevsINIPath        = "/var/local/emhttp/devs.ini"
	defaultSMARTCacheDir      = "/var/local/emhttp/smart"
	defaultUnraidVarINIPath   = "/var/local/emhttp/var.ini"
	defaultPollAttributes     = 30 * time.Second
	diskWatchdogInterval      = 5 * time.Second
	diskSnapshotTimeout       = 3 * diskWatchdogInterval
	diskWakeMargin            = 5 * time.Second
	emhttpPollMargin          = 15 * time.Second
	minimumSMARTFreshness     = 10 * time.Second
	maximumRecommendedPolling = 60 * time.Second
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

	refreshMu              sync.Mutex
	mu                     sync.RWMutex
	err                    error
	updatedAt              time.Time
	errorAt                time.Time
	state                  diskStateTracker
	policyLog              stickyErrorLog
	smartSource            smartSourceState
	fallbackLog            stickyErrorLog
	lastSuccessfulSnapshot []diskRuntimeDisk
	publishedSource        smartSourceStatus

	pollLogInitialized bool
	lastPollInterval   time.Duration
	lastPollError      string
}

type diskRuntimeDisk struct {
	disk           unraidDisk
	reading        sensors.Disk
	hasReading     bool
	observation    diskObservation
	hasObservation bool
	state          diskState
	reused         bool
}

type diskCollectorStatus struct {
	updatedAt time.Time
	errorAt   time.Time
	err       error
	disks     []diskRuntimeDisk
	source    smartSourceStatus
}

func newDiskCollector(paths diskDataPaths) *diskCollector {
	return &diskCollector{
		paths: paths, watchdog: diskWatchdogInterval, now: time.Now,
		err:             errors.New("disk temperatures have not been collected yet"),
		state:           make(diskStateTracker),
		policyLog:       stickyErrorLog{context: "disk policies"},
		fallbackLog:     stickyErrorLog{context: "direct SMART fallback"},
		publishedSource: smartSourceStatus{source: diskSourceEmhttpd},
	}
}

func (c *diskCollector) run(ctx context.Context, refresh <-chan struct{}) {
	runDiskRefreshLoop(ctx, refresh, c.watchdog, func() { c.refreshWithContext(ctx) })
}

func (c *diskCollector) noteEmhttpPoll() {
	c.smartSource.noteEmhttpPoll(c.now())
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
	decision := c.smartSource.evaluate(now, pollInterval, configErr)
	if decision.justEntered {
		log.Printf("emhttpd SMART polling stale, enabling direct SMART fallback")
	}
	if decision.justRecovered {
		log.Printf("emhttpd SMART polling recovered, disabling direct SMART fallback")
		c.fallbackLog.update(nil)
	}

	disks, err := c.readInventory()
	if err != nil {
		c.publishDiskFailure(err, now, c.smartSource.status())
		return
	}

	readings, observations, reused := c.collectTemperatures(ctx, disks, decision, now, pollInterval)
	runtimeDisks := buildDiskRuntimeSnapshot(disks, readings, observations, c.state, reused)
	c.publishDiskSuccess(runtimeDisks, now, c.smartSource.status())
}

func (c *diskCollector) readInventory() ([]unraidDisk, error) {
	policyFile := c.paths.policyFile
	if policyFile == "" {
		policyFile = defaultDiskPolicyFile
	}
	policies, policyErr := readDiskPolicies(policyFile)
	c.policyLog.update(policyErr)
	selector := &diskSelector{sysBlockRoot: c.paths.sysBlockRoot, policies: policies}
	return readDiskInventory(c.paths.disksINI, c.paths.devsINI, selector)
}

func (c *diskCollector) collectTemperatures(
	ctx context.Context,
	disks []unraidDisk,
	decision smartSourceDecision,
	now time.Time,
	pollInterval time.Duration,
) ([]sensors.Disk, []diskObservation, bool) {
	if decision.source == diskSourceEmhttpd {
		observations := makeDiskObservations(disks, c.paths.smartDir, now, smartFreshnessWindow(pollInterval))
		return c.state.apply(observations, now, pollInterval+diskWakeMargin), observations, false
	}
	if !decision.directDue {
		return c.reuseFallbackReadings(disks), nil, true
	}
	c.smartSource.beginDirectAttempt(now)
	observations, err := c.collectFallback(ctx, disks)
	c.smartSource.recordFallbackResult(now, err)
	c.fallbackLog.update(err)
	return c.state.apply(observations, now, pollInterval+diskWakeMargin), observations, false
}

func (c *diskCollector) publishDiskFailure(err error, now time.Time, source smartSourceStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	c.errorAt = now
	c.updatedAt = time.Time{}
	c.publishedSource = source
}

func (c *diskCollector) publishDiskSuccess(runtimeDisks []diskRuntimeDisk, now time.Time, source smartSourceStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = nil
	c.errorAt = time.Time{}
	c.updatedAt = now
	c.lastSuccessfulSnapshot = runtimeDisks
	c.publishedSource = source
}

// reuseFallbackReadings preserves measurements between direct SMART polls
// without treating them as newly collected. Inventory and policy changes are
// still reflected on each watchdog tick; new disks remain unavailable until
// the next direct poll.
func (c *diskCollector) reuseFallbackReadings(disks []unraidDisk) []sensors.Disk {
	c.mu.RLock()
	previous := make(map[string]sensors.Disk, len(c.lastSuccessfulSnapshot))
	if c.err == nil {
		for _, runtime := range c.lastSuccessfulSnapshot {
			if runtime.hasReading {
				previous[runtime.reading.ID] = runtime.reading
			}
		}
	}
	c.mu.RUnlock()

	readings := make([]sensors.Disk, 0, len(disks))
	present := make(map[string]struct{}, len(disks))
	for _, disk := range disks {
		present[disk.id] = struct{}{}
		reading, ok := previous[disk.id]
		if !ok || reading.Device != disk.device || reading.Transport != disk.transport || reading.Rotational != disk.rotational {
			reading.Temp = 0
			reading.Unavailable = true
			delete(c.state, disk.id)
		}
		reading.ID, reading.Name, reading.Device = disk.id, disk.name, disk.device
		reading.Transport, reading.Rotational = disk.transport, disk.rotational
		readings = append(readings, reading)
	}
	for id := range c.state {
		if _, ok := present[id]; !ok {
			delete(c.state, id)
		}
	}
	return readings
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
	return diskReadingsFromRuntime(c.lastSuccessfulSnapshot), nil
}

func diskReadingsFromRuntime(runtimeDisks []diskRuntimeDisk) []sensors.Disk {
	if runtimeDisks == nil {
		return nil
	}
	readings := make([]sensors.Disk, 0, len(runtimeDisks))
	for _, runtime := range runtimeDisks {
		if runtime.hasReading {
			readings = append(readings, runtime.reading)
		}
	}
	return readings
}

func (c *diskCollector) status() diskCollectorStatus {
	c.mu.RLock()
	result := diskCollectorStatus{
		updatedAt: c.updatedAt,
		errorAt:   c.errorAt,
		err:       c.err,
		disks:     slices.Clone(c.lastSuccessfulSnapshot),
		source:    c.publishedSource,
	}
	c.mu.RUnlock()

	liveSource := c.smartSource.status()
	// Heartbeats are independent signals and were historically observable even
	// while a collection was in progress. Collection decisions remain the
	// atomically published state from the last completed refresh.
	result.source.heartbeatSeen = liveSource.heartbeatSeen
	result.source.lastHeartbeat = liveSource.lastHeartbeat
	return result
}

func buildDiskRuntimeSnapshot(
	disks []unraidDisk,
	readings []sensors.Disk,
	observations []diskObservation,
	states diskStateTracker,
	reused bool,
) []diskRuntimeDisk {
	readingsByID := make(map[string]sensors.Disk, len(readings))
	for _, reading := range readings {
		readingsByID[reading.ID] = reading
	}
	observationsByID := make(map[string]diskObservation, len(observations))
	for _, observation := range observations {
		observationsByID[observation.disk.id] = observation
	}

	result := make([]diskRuntimeDisk, 0, len(disks))
	for _, disk := range disks {
		reading, hasReading := readingsByID[disk.id]
		observation, hasObservation := observationsByID[disk.id]
		result = append(result, diskRuntimeDisk{
			disk: disk, reading: reading, hasReading: hasReading,
			observation: observation, hasObservation: hasObservation,
			state: states[disk.id], reused: reused,
		})
	}
	return result
}
