package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

const defaultSnapshotSocket = "/run/unraid-vsock-sensors/sensors.sock"

type snapshotStore struct {
	mu         sync.RWMutex
	response   sensors.Response
	receivedAt time.Time
}

func (s *snapshotStore) set(response sensors.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.response = cloneResponse(response)
	s.receivedAt = time.Now()
}

func (s *snapshotStore) get() sensors.Response {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.receivedAt.IsZero() {
		return sensors.Response{
			Version:  version,
			Error:    "no Unraid snapshot received yet",
			HBAError: "no Unraid snapshot received yet",
		}
	}
	response := cloneResponse(s.response)
	if time.Since(s.receivedAt) >= snapshotStreamTimeout {
		response.Error = "Unraid snapshot stream expired"
		response.HBAError = "Unraid snapshot stream expired"
	}
	return response
}

func cloneResponse(response sensors.Response) sensors.Response {
	response.Disks = append([]sensors.Disk(nil), response.Disks...)
	response.HBAs = append([]sensors.HBA(nil), response.HBAs...)
	return response
}

func listenSnapshotSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create snapshot socket directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("snapshot socket path exists and is not a socket: %s", path)
		}
		probe, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return nil, fmt.Errorf("snapshot socket is already active: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale snapshot socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect snapshot socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on snapshot socket: %w", err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("set snapshot socket permissions: %w", err)
	}
	return listener, nil
}

func serveSnapshotSocket(ctx context.Context, listener net.Listener, store *snapshotStore) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handleSnapshotRequest(conn, store)
	}
}

func handleSnapshotRequest(conn net.Conn, store *snapshotStore) {
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(requestTimeout)); err != nil {
		return
	}
	_ = json.NewEncoder(conn).Encode(store.get())
}
