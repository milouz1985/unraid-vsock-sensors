// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// controlServer is the daemon's local control API. It listens on a Unix socket
// and is the channel for explicit control operations: management inventory
// (GET /v1/disks), disk policies (GET/PUT/DELETE), and manual refresh
// (POST /v1/refresh).
//
// The emhttpd poll_attributes heartbeat is not carried over this socket: it is
// frequent and carries no data, so it is delivered out of band (SIGUSR2) to the
// collector. See newControlServer for details.
//
// The server owns the disk policy store and the read-only inputs needed to
// build the management inventory. Policy mutations are served from the
// persisted file and the inventory is read on demand, so a PUT/DELETE is
// reflected by the next GET without waiting for an asynchronous thermal
// refresh.
type controlServer struct {
	socketPath   string
	policies     *diskPolicyStore
	refresh      chan<- struct{}
	disksINIPath string
	devsINIPath  string
	sysBlockRoot string

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
}

// newControlServer wires the control API to the daemon. refresh is the
// write-only channel through which the server requests a disk collection;
// policyFile is the persistent policy store used both for mutations and for
// management reads; the *Path fields are the read-only
// inputs used to build the management inventory. The emhttpd heartbeat is not
// carried over this socket: it is delivered to the collector out of band
// (SIGUSR2) because it is frequent and carries no data.
func newControlServer(socketPath string, refresh chan<- struct{}, policyFile string, disksINIPath, devsINIPath, sysBlockRoot string) *controlServer {
	if policyFile == "" {
		policyFile = defaultDiskPolicyFile
	}
	if disksINIPath == "" {
		disksINIPath = defaultDisksINIPath
	}
	if devsINIPath == "" {
		devsINIPath = defaultDevsINIPath
	}
	if sysBlockRoot == "" {
		sysBlockRoot = defaultSysBlockRoot
	}
	return &controlServer{
		socketPath:   socketPath,
		policies:     newDiskPolicyStore(policyFile),
		refresh:      refresh,
		disksINIPath: disksINIPath,
		devsINIPath:  devsINIPath,
		sysBlockRoot: sysBlockRoot,
	}
}

// prepareSocket clears a stale socket before listening. A regular file, or a
// socket that a live process answers on (another active instance), is an
// error. Only a socket whose dial is refused by the kernel (no process bound,
// for example a leftover from a hard crash) is removed. Any other probe
// outcome (timeout, permission, ...) is treated as unknown and the path is
// left untouched.
func (s *controlServer) prepareSocket() error {
	info, err := os.Lstat(s.socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket %q: %w", s.socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %q is not a socket", s.socketPath)
	}
	// A socket already exists: if a daemon answers on it, another instance is
	// active and this one must fail.
	dialer := &net.Dialer{Timeout: controlServerProbeTimeout}
	conn, dialErr := dialer.Dial("unix", s.socketPath)
	if dialErr == nil {
		conn.Close()
		return fmt.Errorf("another unraid-vsock-sensors instance is already listening on %s", s.socketPath)
	}
	// Only a kernel connection refusal proves the socket is stale. A timeout or
	// permission error does not, so the path is preserved.
	if isUnixConnRefused(dialErr) {
		if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale control socket %q: %w", s.socketPath, err)
		}
	} else if !errors.Is(dialErr, os.ErrNotExist) {
		log.Printf("control socket %q exists but cannot be probed (%v); leaving it in place", s.socketPath, dialErr)
	}
	return nil
}

// isUnixConnRefused reports whether the dial failed with a kernel
// ECONNREFUSED: a Unix socket with no listening process. This is the only
// outcome that safely proves a preexisting socket is stale.
func isUnixConnRefused(err error) bool {
	var errno unix.Errno
	return errors.As(err, &errno) && errno == unix.ECONNREFUSED
}

const (
	// controlServerProbeTimeout bounds the startup dial in prepareSocket used to
	// detect another live instance on a preexisting socket.
	controlServerProbeTimeout = 500 * time.Millisecond

	// controlServerReadTimeout bounds reading a complete local HTTP request,
	// including its body. Bodies are tiny and additionally size-limited, so a
	// client that cannot finish a request within this window is considered
	// stalled. Do not set WriteTimeout here: an accepted policy mutation may
	// legitimately spend most of its budget waiting for /boot to sync.
	controlServerReadTimeout = 5 * time.Second

	// controlServerShutdownTimeout must be longer than the client mutation
	// budget. Shutdown waits for an accepted policy mutation to finish its
	// fsync/rename before the daemon closes the listener and exits.
	controlServerShutdownTimeout = 7 * time.Second

	// Unexpected listener failures are control-plane failures, not thermal
	// data-plane failures. Retry quickly once, then back off to avoid log/bind
	// loops if the socket path remains unavailable.
	controlServerRetryInitial = 250 * time.Millisecond
	controlServerRetryMax     = 5 * time.Second

	// controlRequestBodyLimit bounds the only JSON mutation body accepted by
	// the local API. The payload is normally below a few hundred bytes; 4 KiB
	// leaves ample room for future fields without allowing an unbounded read.
	controlRequestBodyLimit int64 = 4 << 10
)

// openListener creates and configures the control socket. Startup and recovery
// use the same path so a transient listener failure does not leave the daemon
// without a control plane permanently.
func (s *controlServer) openListener() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0755); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}
	if err := s.prepareSocket(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %q: %w", s.socketPath, err)
	}
	// net.UnixListener removes the socket pathname when it is closed, so a
	// clean shutdown or a failed Serve attempt removes the path before retry.
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		listener.Close()
		return nil, fmt.Errorf("control socket listener is not a Unix listener: %T", listener)
	}
	unixListener.SetUnlinkOnClose(true)
	if err := os.Chmod(s.socketPath, 0660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod control socket %q: %w", s.socketPath, err)
	}
	return listener, nil
}

func (s *controlServer) newHTTPServer() *http.Server {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return &http.Server{
		Handler:           mux,
		ReadTimeout:       controlServerReadTimeout,
		ReadHeaderTimeout: controlServerReadTimeout,
	}
}

// activate installs the listener/server pair currently owned by the serve
// loop. stop marks the server as stopping under the same mutex, which prevents
// a recovery attempt from publishing a new listener after shutdown has begun.
func (s *controlServer) activate(server *http.Server, listener net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.server = server
	s.listener = listener
	return true
}

func (s *controlServer) deactivate(server *http.Server, listener net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == server {
		s.server = nil
	}
	if s.listener == listener {
		s.listener = nil
	}
}

// start begins serving on the control socket. The initial bind is synchronous:
// a bad path, an active second instance or a permission error still prevents
// daemon startup. Once startup succeeded, an unexpected Serve failure is
// isolated to the control plane. The serve loop recreates the socket with
// bounded backoff while disk/HBA collection and VSOCK publication keep running.
func (s *controlServer) start() error {
	listener, err := s.openListener()
	if err != nil {
		return err
	}
	server := s.newHTTPServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.server = server
	s.listener = listener
	s.cancel = cancel
	s.done = done
	s.stopping = false
	s.mu.Unlock()

	go s.serveLoop(ctx, server, listener, done)
	log.Printf("control API listening on %s", s.socketPath)
	return nil
}

func (s *controlServer) serveLoop(ctx context.Context, server *http.Server, listener net.Listener, done chan<- struct{}) {
	defer close(done)
	retryDelay := controlServerRetryInitial

	for {
		serveErr := server.Serve(listener)
		s.deactivate(server, listener)
		_ = listener.Close()
		if ctx.Err() != nil {
			return
		}

		log.Printf("control server stopped unexpectedly: %v; retrying", serveErr)
		for {
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			nextListener, err := s.openListener()
			if err != nil {
				log.Printf("control server recovery failed: %v; retrying in %s", err, nextControlServerRetryDelay(retryDelay))
				retryDelay = nextControlServerRetryDelay(retryDelay)
				continue
			}
			nextServer := s.newHTTPServer()
			if !s.activate(nextServer, nextListener) {
				_ = nextListener.Close()
				return
			}
			server = nextServer
			listener = nextListener
			retryDelay = controlServerRetryInitial
			log.Printf("control API recovered on %s", s.socketPath)
			break
		}
	}
}

func nextControlServerRetryDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > controlServerRetryMax {
		return controlServerRetryMax
	}
	return next
}

// stop shuts the current server down synchronously and prevents the recovery
// loop from creating another listener. With UnlinkOnClose set, closing the
// listener removes the socket file. Calls are idempotent and wait for the
// supervision goroutine to exit before returning.
func (s *controlServer) stop() {
	s.mu.Lock()
	s.stopping = true
	cancel := s.cancel
	server := s.server
	listener := s.listener
	done := s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if server != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), controlServerShutdownTimeout)
		_ = server.Shutdown(shutdownCtx)
		shutdownCancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	if done != nil {
		<-done
	}
}

func (s *controlServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/disks", s.handleListDisks)
	mux.HandleFunc("GET /v1/policies", s.handleValidatePolicies)
	mux.HandleFunc("PUT /v1/disk-policy", s.handleSetDiskPolicy)
	mux.HandleFunc("DELETE /v1/disk-policies", s.handleResetDiskPolicies)
	mux.HandleFunc("POST /v1/refresh", s.handleRefresh)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// decodeJSONBody decodes exactly one bounded JSON value. Control requests are
// local and small, so accepting unknown fields, trailing values or an unbounded
// body would only hide client mistakes and make the API harder to reason about.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, controlRequestBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

// managementInventory builds the disk policy view served to the WebUI and to
// `disks list`. It is read on demand from the current policy file and the
// Unraid inventory files with validateIncluded=false so that an incomplete or
// invalid disk still appears and can be excluded, matching the pre-control
// `disks list` behavior. It does not depend on the collector's thermal state.
func (s *controlServer) managementInventory() ([]diskPolicyRow, error) {
	policies, policyErr := readDiskPolicies(s.policies.path)
	if policyErr != nil {
		return nil, policyErr
	}
	selector := &diskSelector{sysBlockRoot: s.sysBlockRoot, policies: policies}
	entries, err := readDiskInventoryEntries(s.disksINIPath, s.devsINIPath, selector, false)
	if err != nil {
		return nil, err
	}
	return inventoryRowsFromEntries(entries), nil
}

func (s *controlServer) handleListDisks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.managementInventory()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrInvalidPoliciesFile) {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleValidatePolicies checks only the persisted policy file. Unlike
// /v1/disks it does not build the inventory and does not depend on the
// collector's thermal state, so it succeeds even when no disk is readable.
func (s *controlServer) handleValidatePolicies(w http.ResponseWriter, r *http.Request) {
	if err := s.policies.Validate(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrInvalidPoliciesFile) {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *controlServer) handleSetDiskPolicy(w http.ResponseWriter, r *http.Request) {
	var request diskPolicySetRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if request.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.policies.Set(request.ID, request.Policy); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrInvalidDiskID) || errors.Is(err, ErrInvalidDiskPolicy) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	// The refresh is requested only after the policy file was persisted.
	requestDiskRefresh(s.refresh)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *controlServer) handleResetDiskPolicies(w http.ResponseWriter, r *http.Request) {
	if err := s.policies.Reset(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	requestDiskRefresh(s.refresh)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *controlServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	requestDiskRefresh(s.refresh)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
