// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"github.com/mdlayher/socket"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const (
	publisherStatusConnected    = "connected"
	publisherStatusReconnecting = "reconnecting"
)

type snapshotConnection interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

type snapshotDialer func(context.Context) (snapshotConnection, error)

// dialVSOCK connects the publisher to the host without leaving connection
// establishment outside its context deadline.
func dialVSOCK(ctx context.Context, cid, port uint32) (*socket.Conn, error) {
	conn, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Connect(ctx, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

type publisherRuntimeStatus struct {
	status          string
	hostCID         uint32
	port            uint32
	lastConnectedAt time.Time
	lastPublishedAt time.Time
	lastError       string
	lastErrorAt     time.Time
}

type serviceStatus struct {
	startedAt time.Time
	pid       int
	port      uint32
	backend   hbaBackendMode
	publisher publisherRuntimeStatus
}

// serviceState contains service identity and VSOCK publisher runtime state.
// Diagnostics observe a copy; publishing has no dependency on JSON types.
type serviceState struct {
	mu        sync.RWMutex
	startedAt time.Time
	pid       int
	port      uint32
	backend   hbaBackendMode
	publisher publisherRuntimeStatus
}

func newServiceState(port uint32, backend hbaBackendMode) *serviceState {
	return &serviceState{
		startedAt: time.Now(), pid: os.Getpid(), port: port, backend: backend,
		publisher: publisherRuntimeStatus{status: publisherStatusReconnecting, hostCID: uint32(vsock.Host), port: port},
	}
}

func (s *serviceState) connectedNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher.status = publisherStatusConnected
	s.publisher.lastConnectedAt = time.Now()
	s.publisher.lastError = ""
	s.publisher.lastErrorAt = time.Time{}
}

func (s *serviceState) publishedNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher.lastPublishedAt = time.Now()
}

func (s *serviceState) disconnected(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher.status = publisherStatusReconnecting
	s.publisher.lastError = errorText(err)
	s.publisher.lastErrorAt = time.Now()
}

func (s *serviceState) status() serviceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return serviceStatus{
		startedAt: s.startedAt, pid: s.pid, port: s.port, backend: s.backend,
		publisher: s.publisher,
	}
}

func collectorSnapshot(disks *diskCollector, hbas *hbaCollector) sensors.Response {
	diskReadings, diskErr := disks.snapshot()
	hbaReadings, hbaErr := hbas.snapshot()
	response := sensors.Response{
		Protocol: sensors.ProtocolVersion, Disks: diskReadings, HBAs: hbaReadings,
	}
	if diskErr != nil {
		response.Error = diskErr.Error()
	}
	if hbaErr != nil {
		response.HBAError = hbaErr.Error()
	}
	return response
}

func publishSnapshots(
	ctx context.Context,
	port uint32,
	disks *diskCollector,
	hbas *hbaCollector,
	state *serviceState,
) error {
	dial := func(ctx context.Context) (snapshotConnection, error) {
		return dialVSOCK(ctx, vsock.Host, port)
	}
	return publishSnapshotsWithDialer(ctx, disks, hbas, dial, state)
}

func publishSnapshotsWithDialer(
	ctx context.Context,
	disks *diskCollector,
	hbas *hbaCollector,
	dial snapshotDialer,
	state *serviceState,
) error {
	publishLog := stickyErrorLog{context: "VSOCK publishing"}
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, vsockIOTimeout)
		conn, err := dial(connectCtx)
		cancel()
		if err != nil {
			if state != nil {
				state.disconnected(err)
			}
			publishLog.update(err)
			if !waitFor(ctx, defaultPublishInterval) {
				break
			}
			continue
		}
		if state != nil {
			state.connectedNow()
		}
		for ctx.Err() == nil {
			if err = conn.SetWriteDeadline(time.Now().Add(vsockIOTimeout)); err == nil {
				err = sensors.WriteFrame(conn, collectorSnapshot(disks, hbas))
			}
			if err != nil {
				if state != nil {
					state.disconnected(err)
				}
				_ = conn.Close()
				publishLog.update(err)
				break
			}
			publishLog.update(nil)
			if state != nil {
				state.publishedNow()
			}
			if !waitFor(ctx, defaultPublishInterval) {
				_ = conn.Close()
				return nil
			}
		}
		if ctx.Err() == nil && !waitFor(ctx, defaultPublishInterval) {
			break
		}
	}
	return nil
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
