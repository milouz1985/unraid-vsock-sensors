// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"github.com/mdlayher/vsock"
)

// snapshotStreamTimeout detects a stopped publisher and releases the old
// connection so Unraid can reconnect. The kernel's stale_timeout remains
// the thermal failsafe when snapshots do not resume.
const snapshotStreamTimeout = 3 * defaultPublishInterval

type receivedSnapshot struct {
	response sensors.Response
	// receivedAt is recorded after the complete frame has been read. It bounds
	// only the time spent waiting in the local publication queue.
	receivedAt time.Time
}

func (snapshot receivedSnapshot) expired(now time.Time) bool {
	return !now.Before(snapshot.receivedAt.Add(snapshotStreamTimeout))
}

func receiveSnapshots(
	ctx context.Context,
	listener net.Listener,
	expectedCID uint32,
	out chan receivedSnapshot,
) error {
	streamLog := stickyErrorLog{context: "VSOCK snapshot stream"}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept VSOCK publisher: %w", err)
		}
		peer, ok := conn.RemoteAddr().(*vsock.Addr)
		if !ok || peer.ContextID != expectedCID {
			_ = conn.Close()
			continue
		}
		reader := sensors.NewFrameReader(conn)
		for {
			streamErr := conn.SetReadDeadline(time.Now().Add(snapshotStreamTimeout))
			if streamErr == nil {
				var snapshot sensors.Response
				snapshot, streamErr = reader.Read()
				if streamErr == nil {
					streamLog.update(nil)
					received := receivedSnapshot{response: snapshot, receivedAt: time.Now()}
					if !sendLatestSnapshot(ctx, out, received) {
						_ = conn.Close()
						return nil
					}
					continue
				}
			}
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil
			}
			streamLog.update(streamErr)
			break
		}
	}
}

func sendLatestSnapshot(ctx context.Context, out chan receivedSnapshot, snapshot receivedSnapshot) bool {
	select {
	case out <- snapshot:
		return true
	case <-ctx.Done():
		return false
	default:
	}

	// Keep a single pending snapshot. If publication is temporarily busy, a
	// newer heartbeat supersedes the older one instead of creating a backlog.
	select {
	case <-out:
	default:
	}
	select {
	case out <- snapshot:
		return true
	case <-ctx.Done():
		return false
	}
}
