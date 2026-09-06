package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func (s *snapshotStore) set(response sensors.Response) {
	s.mu.Lock()
	s.response = response
	s.available = true
	s.mu.Unlock()
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
	handleSnapshotRequestWithTimeout(conn, store, requestTimeout)
}

func handleSnapshotRequestWithTimeout(conn net.Conn, store *snapshotStore, timeout time.Duration) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestSize+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if len(line) > maxRequestSize || strings.TrimSpace(line) != "GET" {
		return
	}
	_ = json.NewEncoder(conn).Encode(store.get())
}
