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
	mu        sync.RWMutex
	response  sensors.Response
	available bool
}

func (s *snapshotStore) apply(message sensors.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.available {
		s.response.Error = "no disk snapshot received yet"
		s.response.HBAError = "no HBA snapshot received yet"
	}
	s.response.Version = message.Version
	s.response.Timestamp = message.Timestamp
	switch message.Type {
	case sensors.MessageDisks:
		s.response.Disks = append([]sensors.Disk(nil), message.Disks...)
		s.response.Error = message.Error
	case sensors.MessageHBAs:
		s.response.HBAs = append([]sensors.HBA(nil), message.HBAs...)
		s.response.HBADisabled = message.HBADisabled
		s.response.HBAError = message.HBAError
	case sensors.MessageHeartbeat:
	default:
		return fmt.Errorf("unknown stream message type %q", message.Type)
	}
	s.available = true
	return nil
}

func (s *snapshotStore) get() sensors.Response {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.available {
		return sensors.Response{
			Version:  version,
			Error:    "no Unraid snapshot received yet",
			HBAError: "no Unraid snapshot received yet",
		}
	}
	return cloneResponse(s.response)
}

func (s *snapshotStore) expireDisks() {
	s.mu.Lock()
	s.response.Error = "disk snapshot TTL expired"
	s.mu.Unlock()
}

func (s *snapshotStore) expireHBAs() {
	s.mu.Lock()
	s.response.HBAError = "HBA snapshot TTL expired"
	s.mu.Unlock()
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
		go handleSnapshotRequest(conn, store)
	}
}

func handleSnapshotRequest(conn net.Conn, store *snapshotStore) {
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(requestTimeout)); err != nil {
		return
	}
	_ = json.NewEncoder(conn).Encode(store.get())
}
