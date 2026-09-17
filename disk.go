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
	defaultDisksINIPath     = "/var/local/emhttp/disks.ini"
	defaultDevsINIPath      = "/var/local/emhttp/devs.ini"
	defaultUnraidVarINIPath = "/var/local/emhttp/var.ini"
	diskWatchdogInterval    = 5 * time.Second
	diskSnapshotTimeout     = 3 * diskWatchdogInterval
	diskWakeMargin          = 5 * time.Second
)

type diskDataPaths struct {
	disksINI     string
	devsINI      string
	varINI       string
	sysBlockRoot string
	policyFile   string
	sdspin       string
	smartctlType string
}

var defaultDiskDataPaths = diskDataPaths{
	disksINI: defaultDisksINIPath, devsINI: defaultDevsINIPath,
	varINI:       defaultUnraidVarINIPath,
	sysBlockRoot: defaultSysBlockRoot, policyFile: defaultDiskPolicyFile,
	sdspin: defaultSDSpinPath, smartctlType: defaultSmartctlTypePath,
}

type diskCollector struct {
	paths    diskDataPaths
	watchdog time.Duration
	now      func() time.Time

	refreshMu              sync.Mutex
	fallbackCursor         int // guarded by refreshMu
	mu                     sync.RWMutex
	err                    error
	updatedAt              time.Time
	errorAt                time.Time
	state                  diskStateTracker
	policyLog              stickyErrorLog
	policyError            string
	lastValidPolicies      map[string]diskPolicy // guarded by refreshMu
	haveValidPolicies      bool                  // guarded by refreshMu
	smartSource            smartSourceState
	fallbackLog            stickyErrorLog
	lastSuccessfulSnapshot []diskRuntimeDisk
	publishedSource        smartSourceStatus

	lastValidPollInterval time.Duration // guarded by refreshMu
	haveValidPollInterval bool          // guarded by refreshMu
	pollLogInitialized    bool
	lastPollInterval      time.Duration
	lastPollError         string
}

type diskRuntimeDisk struct {
	disk            unraidDisk
	reading         sensors.Disk
	hasReading      bool
	collectionError error
	state           diskState
	reused          bool
}

type diskCollectorStatus struct {
	updatedAt   time.Time
	errorAt     time.Time
	err         error
	policyError string
	disks       []diskRuntimeDisk
	source      smartSourceStatus
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

func (c *diskCollector) refreshWithContext(ctx context.Context) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	decisionAt := c.now()
	pollInterval, configErr := c.effectivePollAttributes()
	c.logPollAttributesChange(pollInterval, configErr)
	decision := c.smartSource.evaluate(decisionAt, pollInterval, configErr)
	if decision.justEntered {
		log.Printf("emhttpd SMART polling stale, enabling direct SMART fallback")
	}
	if decision.justRecovered {
		log.Printf("emhttpd SMART polling recovered, disabling direct SMART fallback")
		c.fallbackLog.update(nil)
	}

	disks, err := c.readInventory()
	if err != nil {
		finishedAt := c.now()
		c.state.invalidateContinuity()
		c.publishDiskFailure(err, finishedAt, c.smartSource.status())
		return
	}

	readings, observations, reused, fallbackErr := c.collectTemperatures(ctx, disks, decision, decisionAt, pollInterval)
	finishedAt := c.now()
	wakeGrace := time.Duration(0)
	if pollInterval > 0 {
		wakeGrace = pollInterval + diskWakeMargin
	}
	if !reused {
		readings = c.state.apply(observations, finishedAt, wakeGrace)
	}
	if decision.source == diskSourceDirect && decision.directDue {
		c.smartSource.recordFallbackResult(finishedAt, fallbackErr)
		c.fallbackLog.update(fallbackErr)
	}
	runtimeDisks := c.buildDiskRuntimeSnapshot(disks, readings, observations, reused)
	c.publishDiskSuccess(runtimeDisks, finishedAt, c.smartSource.status())
}

func (c *diskCollector) readInventory() ([]unraidDisk, error) {
	policyFile := c.paths.policyFile
	if policyFile == "" {
		policyFile = defaultDiskPolicyFile
	}
	policies, policyErr := readDiskPolicies(policyFile)
	if policyErr == nil {
		c.lastValidPolicies = policies
		c.haveValidPolicies = true
	} else if c.haveValidPolicies {
		// A transient read or parse failure must not change the effective disk
		// selection. Keep using the last configuration that was known to be
		// valid while exposing the current file error through diagnostics.
		policies = c.lastValidPolicies
	}
	c.policyLog.update(policyErr)
	c.mu.Lock()
	c.policyError = errorText(policyErr)
	c.mu.Unlock()
	selector := &diskSelector{sysBlockRoot: c.paths.sysBlockRoot, policies: policies}
	return readDiskInventory(c.paths.disksINI, c.paths.devsINI, selector)
}

func (c *diskCollector) collectTemperatures(
	ctx context.Context,
	disks []unraidDisk,
	decision smartSourceDecision,
	decisionAt time.Time,
	pollInterval time.Duration,
) ([]sensors.Disk, []diskObservation, bool, error) {
	if decision.source == diskSourceEmhttpd {
		observations := makeDiskObservations(disks, pollInterval > 0)
		return nil, observations, false, nil
	}
	if !decision.directDue {
		return c.reuseFallbackReadings(disks), nil, true, nil
	}
	c.smartSource.beginDirectAttempt(decisionAt)
	observations, err := c.collectFallback(ctx, disks)
	return nil, observations, false, err
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
	previous := make(map[string]diskRuntimeDisk, len(c.lastSuccessfulSnapshot))
	for _, runtime := range c.lastSuccessfulSnapshot {
		previous[runtime.disk.id] = runtime
	}
	canReuse := c.err == nil
	c.mu.RUnlock()

	readings := make([]sensors.Disk, 0, len(disks))
	present := make(map[string]struct{}, len(disks))
	for _, disk := range disks {
		present[disk.id] = struct{}{}
		runtime := previous[disk.id]
		reading := runtime.reading
		ok := canReuse && runtime.hasReading
		if !ok {
			reading.Temp = 0
			reading.Unavailable = true
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
		updatedAt:   c.updatedAt,
		errorAt:     c.errorAt,
		err:         c.err,
		policyError: c.policyError,
		disks:       slices.Clone(c.lastSuccessfulSnapshot),
		source:      c.publishedSource,
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

func (c *diskCollector) buildDiskRuntimeSnapshot(
	disks []unraidDisk,
	readings []sensors.Disk,
	observations []diskObservation,
	reused bool,
) []diskRuntimeDisk {
	readingsByID := make(map[string]sensors.Disk, len(readings))
	for _, reading := range readings {
		readingsByID[reading.ID] = reading
	}
	var errorsByID map[string]error
	if !reused {
		errorsByID = make(map[string]error, len(observations))
		for _, observation := range observations {
			errorsByID[observation.disk.id] = observation.err
		}
	}

	// When reusing fallback readings, observations is nil. Preserve the
	// collection error from the previous snapshot so diagnostics continue
	// to show why a disk is unavailable between direct SMART polls. The error
	// is only preserved for the same stable ID, matching the criterion used
	// by reuseFallbackReadings() to retain measurements.
	var previousByDisk map[string]diskRuntimeDisk
	if reused {
		c.mu.RLock()
		previousByDisk = make(map[string]diskRuntimeDisk, len(c.lastSuccessfulSnapshot))
		for _, runtime := range c.lastSuccessfulSnapshot {
			previousByDisk[runtime.disk.id] = runtime
		}
		c.mu.RUnlock()
	}

	result := make([]diskRuntimeDisk, 0, len(disks))
	for _, disk := range disks {
		reading, hasReading := readingsByID[disk.id]
		var collectionError error
		if reused {
			collectionError = previousByDisk[disk.id].collectionError
		} else {
			collectionError = errorsByID[disk.id]
		}
		result = append(result, diskRuntimeDisk{
			disk: disk, reading: reading, hasReading: hasReading,
			collectionError: collectionError,
			state:           c.state[disk.id], reused: reused,
		})
	}
	return result
}
