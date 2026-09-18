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
	"time"

	"golang.org/x/sys/unix"
)

// controlServer is the daemon's local WebUI API. It listens on a Unix socket
// and exposes only disk inventory plus disk-policy mutations.
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
	socketPath string
	policies   *diskPolicyStore
	refresh    chan<- struct{}
	paths      diskDataPaths

	server *http.Server
	done   chan struct{}
}

// newControlServer wires the control API to the daemon. refresh is the
// write-only channel through which the server requests a disk collection. The
// server shares the collector's disk data paths so management reads and policy
// mutations cannot drift onto a different inventory or policy store. The
// emhttpd heartbeat is delivered to the collector out of band (SIGUSR2) because
// it is frequent and carries no data.
func newControlServer(socketPath string, refresh chan<- struct{}, paths diskDataPaths) *controlServer {
	return &controlServer{
		socketPath: socketPath,
		policies:   newDiskPolicyStore(paths.policyFile),
		refresh:    refresh,
		paths:      paths,
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
	defaultControlSocketPath = "/run/unraid-vsock-sensors/control.sock"

	// controlServerProbeTimeout bounds the startup probe used to distinguish a
	// stale Unix socket from another live daemon instance.
	controlServerProbeTimeout = 500 * time.Millisecond

	// Requests are local and tiny. Bound reads so a broken client cannot keep a
	// handler occupied forever. Writes are intentionally unbounded because a
	// policy mutation may spend several seconds syncing /boot.
	controlServerReadTimeout = 5 * time.Second

	// Give an accepted policy write enough time to complete during daemon stop.
	controlServerShutdownTimeout = 7 * time.Second

	// The only request body is a small disk-policy mutation.
	controlRequestBodyLimit int64 = 4 << 10
)

// openListener creates and configures the control socket.
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
	if err := os.Chmod(s.socketPath, 0660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod control socket %q: %w", s.socketPath, err)
	}
	return listener, nil
}

// start binds the Unix socket synchronously, then serves it in its own
// goroutine. Failure to create the initial socket prevents daemon startup. A
// later Serve failure is logged but deliberately does not stop the thermal
// data plane; restarting the service recreates the control socket.
func (s *controlServer) start() error {
	listener, err := s.openListener()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	server := &http.Server{Handler: mux, ReadTimeout: controlServerReadTimeout}
	done := make(chan struct{})
	s.server = server
	s.done = done

	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("control server stopped unexpectedly: %v; restart the service to restore the WebUI control plane", err)
		}
	}()
	log.Printf("control API listening on %s", s.socketPath)
	return nil
}

// stop drains accepted requests before closing the local control socket.
func (s *controlServer) stop() {
	if s.server == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), controlServerShutdownTimeout)
	// Shutdown closes the listener before waiting for active handlers.
	_ = s.server.Shutdown(shutdownCtx)
	cancel()
	if s.done != nil {
		<-s.done
	}
}

func (s *controlServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/disks", s.handleListDisks)
	mux.HandleFunc("PUT /v1/disk-policy", s.handleSetDiskPolicy)
	mux.HandleFunc("DELETE /v1/disk-policies", s.handleResetDiskPolicies)
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

// managementInventory builds the disk policy view served to the WebUI. It is
// read on demand from the current policy file and the Unraid inventory files.
// Inventory parsing always preserves incomplete rows and their eligibility so
// the WebUI can still exclude them. It does not depend on the collector's
// thermal state.
func (s *controlServer) managementInventory() ([]diskPolicyRow, error) {
	policies, policyErr := readDiskPolicies(s.policies.path)
	if policyErr != nil {
		return nil, policyErr
	}
	selector := &diskSelector{sysBlockRoot: s.paths.sysBlockRoot, policies: policies}
	entries, err := readDiskInventoryEntries(s.paths.disksINI, s.paths.devsINI, selector)
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

type diskPolicySetRequest struct {
	ID     string     `json:"id"`
	Policy diskPolicy `json:"policy"`
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
