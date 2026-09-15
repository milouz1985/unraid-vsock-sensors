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
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"golang.org/x/sys/unix"
)

const (
	defaultDiagnosticsPath    = "/run/unraid-vsock-sensors/diagnostics.json"
	diagnosticsMaxAge         = 5 * time.Second
	diagnosticStatusHealthy   = "healthy"
	diagnosticStatusStale     = "stale"
	diagnosticStatusError     = "error"
	diagnosticStatusDisabled  = "disabled"
	diagnosticDiskValid       = "valid"
	diagnosticDiskRetained    = "retained"
	diagnosticDiskUnavailable = "unavailable"
	diagnosticDiskStandby     = "standby"
	diagnosticDiskWaking      = "waking"
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
	Status      string           `json:"status"`
	UpdatedAt   *time.Time       `json:"updated_at,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorAt     *time.Time       `json:"error_at,omitempty"`
	PolicyError string           `json:"policy_error,omitempty"`
	Items       []diagnosticDisk `json:"items"`
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

func buildDiagnosticsSnapshot(service serviceStatus, disks diskCollectorStatus, hbas hbaCollectorStatus, now time.Time) diagnosticsSnapshot {
	vsock := buildDiagnosticVSOCK(service.publisher, now)
	diagnosticDisks, emhttpd := buildDiagnosticDiskServices(disks, now)
	hba := buildDiagnosticHBA(hbas, service.backend, now)
	return diagnosticsSnapshot{
		SchemaVersion: 1, Version: version, PID: service.pid, StartedAt: service.startedAt, GeneratedAt: now,
		Service: "running", UptimeSeconds: int64(max(0, now.Sub(service.startedAt).Seconds())),
		Config: diagnosticConfig{
			VSOCKPort: service.port, HBAMode: hbas.mode, HBABackend: service.backend, HBAInterval: hbas.interval.String(),
			PollAttributes: emhttpd.PollAttributes, EmhttpdStaleAfter: emhttpd.StaleAfter,
		},
		VSOCK: vsock, Emhttpd: emhttpd, Disks: diagnosticDisks, HBA: hba,
	}
}

func buildDiagnosticVSOCK(publisher publisherRuntimeStatus, now time.Time) diagnosticVSOCK {
	result := diagnosticVSOCK{
		Status: publisher.status, HostCID: publisher.hostCID, Port: publisher.port,
		LastConnectedAt: timePointer(publisher.lastConnectedAt),
		LastPublishedAt: timePointer(publisher.lastPublishedAt),
		LastError:       publisher.lastError, LastErrorAt: timePointer(publisher.lastErrorAt),
	}
	result.LastPublishedAgeSeconds = ageSeconds(result.LastPublishedAt, now)
	return result
}

func buildDiagnosticDiskServices(disks diskCollectorStatus, now time.Time) (diagnosticDisks, diagnosticEmhttpd) {
	pollInterval := disks.source.pollInterval
	if pollInterval == 0 && !disks.source.initialized {
		pollInterval = defaultPollAttributes
	}
	staleAfter := "disabled"
	if pollInterval > 0 {
		staleAfter = (pollInterval + emhttpPollMargin).String()
	}
	fallback := disks.source.source == diskSourceDirect
	diskResult := diagnosticDisks{
		Status: diagnosticStatusHealthy, UpdatedAt: timePointer(disks.updatedAt), Error: errorText(disks.err),
		ErrorAt: timePointer(disks.errorAt), PolicyError: disks.policyError, Items: buildDiagnosticDisks(disks.disks),
	}
	if disks.err != nil {
		diskResult.Status = diagnosticStatusError
	} else if disks.updatedAt.IsZero() || !now.Before(disks.updatedAt.Add(diskSnapshotTimeout)) {
		diskResult.Status = diagnosticStatusStale
	}
	if diskResult.Status != diagnosticStatusHealthy {
		for i := range diskResult.Items {
			if diskResult.Items[i].Status == diagnosticDiskValid {
				diskResult.Items[i].Status = diagnosticDiskRetained
			}
		}
	}
	for i := range diskResult.Items {
		diskResult.Items[i].LastValidAgeSeconds = ageSeconds(diskResult.Items[i].LastValidAt, now)
	}

	emhttpd := diagnosticEmhttpd{
		Status: diagnosticStatusHealthy, PollAttributes: pollInterval.String(), StaleAfter: staleAfter,
		TemperatureSource: string(disks.source.source), FallbackActive: fallback,
		FallbackSince:                 timePointer(disks.source.fallbackSince),
		LastFallbackAttemptAt:         timePointer(disks.source.lastObservedAttempt),
		LastFallbackAttemptAgeSeconds: ageSeconds(timePointer(disks.source.lastObservedAttempt), now),
		Error:                         disks.source.configError, FallbackError: disks.source.lastFallbackError,
		FallbackErrorAt: timePointer(disks.source.fallbackErrorAt),
	}
	if disks.source.heartbeatSeen {
		emhttpd.LastPollAt = timePointer(disks.source.lastHeartbeat)
		emhttpd.LastPollAgeSeconds = ageSeconds(emhttpd.LastPollAt, now)
	}
	switch {
	case disks.source.configError != "":
		emhttpd.Status = "unknown/config error"
	case pollInterval == 0:
		emhttpd.Status = "polling disabled"
	case fallback:
		emhttpd.Status = diagnosticStatusStale
	case emhttpd.LastPollAt == nil:
		emhttpd.Status = "unknown"
	}
	return diskResult, emhttpd
}

func buildDiagnosticHBA(hbas hbaCollectorStatus, backend hbaBackendMode, now time.Time) diagnosticHBA {
	result := diagnosticHBA{Mode: hbas.mode, Backend: backend, Interval: hbas.interval.String(),
		LastSuccessfulAt: timePointer(hbas.lastSuccessfulAt), LastError: errorText(hbas.err),
		LastErrorAt: timePointer(hbas.lastErrorAt), Items: hbas.lastSuccessfulSnapshot}
	if hbas.mode == hbaModeDisabled {
		result.Status = diagnosticStatusDisabled
		result.LastError = ""
	} else if hbas.err != nil {
		result.Status = diagnosticStatusError
	} else if hbas.updatedAt.IsZero() || !now.Before(hbas.updatedAt.Add(hbas.interval+hbaCollectionTimeout)) {
		result.Status = diagnosticStatusStale
	} else {
		result.Status = diagnosticStatusHealthy
	}
	result.SnapshotAgeSeconds = ageSeconds(result.LastSuccessfulAt, now)
	result.Count = len(result.Items)
	return result
}

func buildDiagnosticDisks(disks []diskRuntimeDisk) []diagnosticDisk {
	if disks == nil {
		return nil
	}
	items := make([]diagnosticDisk, 0, len(disks))
	for _, runtime := range disks {
		disk := runtime.disk
		state := runtime.state
		item := diagnosticDisk{Name: disk.name, ID: disk.id, Device: "/dev/" + disk.device,
			Transport: disk.transport, Rotational: disk.rotational, Spundown: disk.spundown,
			SMARTCacheName: disk.smartName, LastValidAt: timePointer(state.lastValidAt),
			SMARTCacheAt: timePointer(state.cacheAt), Source: string(state.lastSource), Status: diagnosticDiskValid}
		if runtime.hasReading {
			if runtime.reading.Unavailable {
				item.Status = diagnosticDiskUnavailable
			} else if runtime.reused {
				item.Status = diagnosticDiskRetained
			}
			if !runtime.reading.Unavailable && item.Status != diagnosticDiskStandby {
				value := runtime.reading.Temp
				item.Temperature = &value
			}
		}
		if runtime.hasObservation {
			observation := runtime.observation
			if observation.standby {
				item.Status = diagnosticDiskStandby
				item.Temperature = nil
			} else if observation.err != nil {
				item.Error = observation.err.Error()
				if item.Status == diagnosticDiskValid {
					item.Status = diagnosticDiskRetained
				}
			}
		}
		switch state.thermalState {
		case diskThermalStandby:
			item.Status = diagnosticDiskStandby
			item.Temperature = nil
		case diskThermalWaking:
			item.Status = diagnosticDiskWaking
			item.Temperature = nil
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
func runDiagnostics(ctx context.Context, path string, state *serviceState, disks *diskCollector, hbas *hbaCollector) {
	logState := stickyErrorLog{context: "diagnostics snapshot"}
	write := func() {
		snapshot := buildDiagnosticsSnapshot(state.status(), disks.status(), hbas.status(), time.Now())
		logState.update(writeDiagnosticsAtomic(path, snapshot))
	}
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
