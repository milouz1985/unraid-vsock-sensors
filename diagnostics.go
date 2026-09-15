// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const (
	defaultDiagnosticsPath = "/run/unraid-vsock-sensors/diagnostics.json"
	diagnosticsMaxAge      = 5 * time.Second
)

type diagnosticsSnapshot struct {
	SchemaVersion int               `json:"schema_version"`
	Version       string            `json:"version"`
	PID           int               `json:"pid"`
	StartedAt     time.Time         `json:"started_at"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Service       string            `json:"service"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Config        diagnosticConfig  `json:"config"`
	VSOCK         diagnosticVSOCK   `json:"vsock"`
	Emhttpd       diagnosticEmhttpd `json:"emhttpd"`
	Disks         diagnosticDisks   `json:"disks"`
	HBA           diagnosticHBA     `json:"hba"`
}

type diagnosticConfig struct {
	VSOCKPort         uint32         `json:"vsock_port"`
	HBAMode           hbaMode        `json:"hba_mode"`
	HBABackend        hbaBackendMode `json:"hba_backend"`
	HBAInterval       string         `json:"hba_interval"`
	PollAttributes    string         `json:"poll_attributes"`
	EmhttpdStaleAfter string         `json:"emhttpd_stale_after"`
}

type diagnosticVSOCK struct {
	Status                  string     `json:"status"`
	HostCID                 uint32     `json:"host_cid"`
	Port                    uint32     `json:"port"`
	LastConnectedAt         *time.Time `json:"last_connected_at,omitempty"`
	LastPublishedAt         *time.Time `json:"last_published_at,omitempty"`
	LastPublishedAgeSeconds *int64     `json:"last_published_age_seconds,omitempty"`
	LastError               string     `json:"last_error,omitempty"`
	LastErrorAt             *time.Time `json:"last_error_at,omitempty"`
}

type diagnosticEmhttpd struct {
	Status                        string     `json:"status"`
	LastPollAt                    *time.Time `json:"last_poll_at,omitempty"`
	LastPollAgeSeconds            *int64     `json:"last_poll_age_seconds,omitempty"`
	PollAttributes                string     `json:"poll_attributes"`
	StaleAfter                    string     `json:"stale_after"`
	TemperatureSource             string     `json:"temperature_source"`
	FallbackActive                bool       `json:"fallback_active"`
	FallbackSince                 *time.Time `json:"fallback_since,omitempty"`
	LastFallbackAttemptAt         *time.Time `json:"last_fallback_attempt_at,omitempty"`
	LastFallbackAttemptAgeSeconds *int64     `json:"last_fallback_attempt_age_seconds,omitempty"`
	Error                         string     `json:"error,omitempty"`
	FallbackError                 string     `json:"fallback_error,omitempty"`
	FallbackErrorAt               *time.Time `json:"fallback_error_at,omitempty"`
}

type diagnosticDisk struct {
	Name                string     `json:"name"`
	ID                  string     `json:"id"`
	Device              string     `json:"device"`
	Transport           string     `json:"transport"`
	Rotational          bool       `json:"rotational"`
	Temperature         *float64   `json:"temperature_c,omitempty"`
	Status              string     `json:"status"`
	Source              string     `json:"source,omitempty"`
	Spundown            bool       `json:"spundown"`
	SMARTCacheName      string     `json:"smart_cache_name,omitempty"`
	SMARTCacheAt        *time.Time `json:"smart_cache_at,omitempty"`
	LastValidAt         *time.Time `json:"last_valid_at,omitempty"`
	LastValidAgeSeconds *int64     `json:"last_valid_age_seconds,omitempty"`
	Error               string     `json:"error,omitempty"`
}

type diagnosticDisks struct {
	Status    string           `json:"status"`
	UpdatedAt *time.Time       `json:"updated_at,omitempty"`
	Error     string           `json:"error,omitempty"`
	ErrorAt   *time.Time       `json:"error_at,omitempty"`
	Items     []diagnosticDisk `json:"items"`
}

type diagnosticHBA struct {
	Status             string         `json:"status"`
	Mode               hbaMode        `json:"mode"`
	Backend            hbaBackendMode `json:"backend"`
	Interval           string         `json:"interval"`
	LastSuccessfulAt   *time.Time     `json:"last_successful_at,omitempty"`
	SnapshotAgeSeconds *int64         `json:"snapshot_age_seconds,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	LastErrorAt        *time.Time     `json:"last_error_at,omitempty"`
	Count              int            `json:"count"`
	Items              []sensors.HBA  `json:"items"`
}

type diagnosticsState struct {
	mu        sync.RWMutex
	startedAt time.Time
	pid       int
	port      uint32
	backend   hbaBackendMode
	vsock     diagnosticVSOCK
}

func newDiagnosticsState(port uint32, backend hbaBackendMode) *diagnosticsState {
	return &diagnosticsState{startedAt: time.Now(), pid: os.Getpid(), port: port, backend: backend,
		vsock: diagnosticVSOCK{Status: "reconnecting", HostCID: uint32(vsock.Host), Port: port}}
}

func (s *diagnosticsState) connectedNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vsock.Status = "connected"
	s.vsock.LastConnectedAt = timePointer(time.Now())
	s.vsock.LastError = ""
	s.vsock.LastErrorAt = nil
}

func (s *diagnosticsState) publishedNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vsock.LastPublishedAt = timePointer(time.Now())
}

func (s *diagnosticsState) disconnected(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vsock.Status = "reconnecting"
	s.vsock.LastError = errorText(err)
	s.vsock.LastErrorAt = timePointer(time.Now())
}

func (s *diagnosticsState) snapshot(disks *diskCollector, hbas *hbaCollector, now time.Time) diagnosticsSnapshot {
	s.mu.RLock()
	result := diagnosticsSnapshot{
		SchemaVersion: 1, Version: version, PID: s.pid, StartedAt: s.startedAt, GeneratedAt: now,
		Service: "running", UptimeSeconds: int64(max(0, now.Sub(s.startedAt).Seconds())),
		Config: diagnosticConfig{VSOCKPort: s.port, HBAMode: hbas.mode, HBABackend: s.backend, HBAInterval: hbas.interval.String()},
		VSOCK:  s.vsock,
	}
	s.mu.RUnlock()
	result.VSOCK.LastPublishedAgeSeconds = ageSeconds(result.VSOCK.LastPublishedAt, now)

	disks.mu.RLock()
	pollInterval := disks.diagnosticPoll
	if pollInterval == 0 && !disks.diagnosticReady {
		pollInterval = defaultPollAttributes
	}
	configErr := disks.diagnosticPollErr
	staleAfter := "disabled"
	if pollInterval > 0 {
		staleAfter = (pollInterval + emhttpPollMargin).String()
	}
	fallback := disks.diagnosticFallback
	lastAttempt := disks.diagnosticAttempt
	fallbackSince := disks.diagnosticFallbackSince
	result.Disks = diagnosticDisks{Status: "healthy", UpdatedAt: timePointer(disks.updatedAt), Error: errorText(disks.err),
		ErrorAt: timePointer(disks.diagnosticErrorAt), Items: slices.Clone(disks.diagnosticDisks)}
	if disks.err != nil {
		result.Disks.Status = "error"
	} else if disks.updatedAt.IsZero() || !now.Before(disks.updatedAt.Add(diskSnapshotTimeout)) {
		result.Disks.Status = "stale"
	}
	if result.Disks.Status != "healthy" {
		for i := range result.Disks.Items {
			if result.Disks.Items[i].Status == "valid" {
				result.Disks.Items[i].Status = "retained"
			}
		}
	}
	result.Emhttpd = diagnosticEmhttpd{
		Status: "healthy", PollAttributes: pollInterval.String(), StaleAfter: staleAfter,
		TemperatureSource: "emhttpd cache", FallbackActive: fallback, FallbackSince: timePointer(fallbackSince),
		LastFallbackAttemptAt: timePointer(lastAttempt), LastFallbackAttemptAgeSeconds: ageSeconds(timePointer(lastAttempt), now),
		Error: configErr, FallbackError: disks.diagnosticFallbackError,
		FallbackErrorAt: timePointer(disks.diagnosticFallbackErrorAt),
	}
	disks.mu.RUnlock()
	result.Config.PollAttributes = result.Emhttpd.PollAttributes
	result.Config.EmhttpdStaleAfter = result.Emhttpd.StaleAfter
	if fallback {
		result.Emhttpd.TemperatureSource = "direct SMART fallback"
	}
	if disks.emhttpPollSeen.Load() {
		if lastPoll := disks.lastEmhttpPoll.Load(); lastPoll != nil {
			result.Emhttpd.LastPollAt = timePointer(*lastPoll)
			result.Emhttpd.LastPollAgeSeconds = ageSeconds(lastPoll, now)
		}
	}
	switch {
	case configErr != "":
		result.Emhttpd.Status = "unknown/config error"
	case pollInterval == 0:
		result.Emhttpd.Status = "polling disabled"
	case fallback:
		result.Emhttpd.Status = "stale"
	case result.Emhttpd.LastPollAt == nil:
		result.Emhttpd.Status = "unknown"
	}
	for i := range result.Disks.Items {
		item := &result.Disks.Items[i]
		item.LastValidAgeSeconds = ageSeconds(item.LastValidAt, now)
	}

	hbas.mu.RLock()
	result.HBA = diagnosticHBA{Mode: hbas.mode, Backend: s.backend, Interval: hbas.interval.String(),
		LastSuccessfulAt: timePointer(hbas.lastSuccessfulAt), LastError: errorText(hbas.err),
		LastErrorAt: timePointer(hbas.lastErrorAt), Items: slices.Clone(hbas.diagnosticReadings)}
	if hbas.mode == hbaModeDisabled {
		result.HBA.Status = "disabled"
		result.HBA.LastError = ""
	} else if hbas.err != nil {
		result.HBA.Status = "error"
	} else if hbas.updatedAt.IsZero() || !now.Before(hbas.updatedAt.Add(hbas.interval+hbaCollectionTimeout)) {
		result.HBA.Status = "stale"
	} else {
		result.HBA.Status = "healthy"
	}
	result.HBA.SnapshotAgeSeconds = ageSeconds(result.HBA.LastSuccessfulAt, now)
	result.HBA.Count = len(result.HBA.Items)
	hbas.mu.RUnlock()
	return result
}

func buildDiagnosticDisks(disks []unraidDisk, readings []sensors.Disk, observations []diskObservation, states diskStateTracker, fallback, reused bool) []diagnosticDisk {
	items := make([]diagnosticDisk, 0, len(disks))
	for i, disk := range disks {
		state := states[disk.id]
		item := diagnosticDisk{Name: disk.name, ID: disk.id, Device: "/dev/" + disk.device,
			Transport: disk.transport, Rotational: disk.rotational, Spundown: disk.spundown,
			SMARTCacheName: disk.smartName, LastValidAt: timePointer(state.lastValidAt),
			SMARTCacheAt: timePointer(state.cacheAt), Source: state.lastSource, Status: "valid"}
		if i < len(readings) {
			if readings[i].Unavailable {
				item.Status = "unavailable"
			} else if reused {
				item.Status = "retained"
			}
			if !readings[i].Unavailable && item.Status != "standby" {
				value := readings[i].Temp
				item.Temperature = &value
			}
		}
		if i < len(observations) {
			observation := observations[i]
			if observation.standby {
				item.Status = "standby"
				item.Temperature = nil
			} else if observation.err != nil {
				item.Error = observation.err.Error()
				if item.Status == "valid" {
					item.Status = "retained"
				}
			}
		}
		if fallback && item.Status == "valid" {
			item.Source = "direct SMART fallback"
		}
		items = append(items, item)
	}
	return items
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}
func ageSeconds(value *time.Time, now time.Time) *int64 {
	if value == nil {
		return nil
	}
	age := int64(max(0, now.Sub(*value).Seconds()))
	return &age
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func runDiagnostics(ctx context.Context, path string, state *diagnosticsState, disks *diskCollector, hbas *hbaCollector) {
	logState := stickyErrorLog{context: "diagnostics snapshot"}
	write := func() { logState.update(writeDiagnosticsAtomic(path, state.snapshot(disks, hbas, time.Now()))) }
	write()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			write()
		}
	}
}

func writeDiagnosticsAtomic(path string, snapshot diagnosticsSnapshot) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".diagnostics-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(snapshot); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func diagnosticsCommand(args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("diagnostics does not accept arguments")
	}
	snapshot, err := readDiagnostics(defaultDiagnosticsPath, time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(snapshot)
}

func readDiagnostics(path string, now time.Time) (diagnosticsSnapshot, error) {
	var snapshot diagnosticsSnapshot
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, errors.New("daemon stopped: diagnostics state is absent")
	}
	if err != nil {
		return snapshot, fmt.Errorf("read diagnostics: %w", err)
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, fmt.Errorf("invalid diagnostics state: %w", err)
	}
	if snapshot.SchemaVersion != 1 || snapshot.PID <= 0 || snapshot.GeneratedAt.IsZero() || snapshot.StartedAt.IsZero() || snapshot.Version == "" {
		return snapshot, errors.New("invalid diagnostics state: required fields are missing")
	}
	err = unix.Kill(snapshot.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return snapshot, errors.New("daemon stopped: diagnostics state belongs to a process that no longer exists")
	}
	if err != nil && !errors.Is(err, unix.EPERM) {
		return snapshot, fmt.Errorf("check daemon process: %w", err)
	}
	if now.Sub(snapshot.GeneratedAt) > diagnosticsMaxAge || snapshot.GeneratedAt.After(now.Add(time.Second)) {
		return snapshot, errors.New("diagnostic state stale: daemon has not updated its runtime snapshot")
	}
	return snapshot, nil
}
