// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// defaultControlSocketPath is the local control socket owned by the daemon.
const defaultControlSocketPath = "/run/unraid-vsock-sensors/control.sock"

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
	policyFile   string
	policies     *diskPolicyStore
	refresh      chan<- struct{}
	disksINIPath string
	devsINIPath  string
	sysBlockRoot string

	server   *http.Server
	listener net.Listener
}

// newControlServer wires the control API to the daemon. refresh is the
// write-only channel through which the server requests a disk collection;
// policyFile is the persistent policy store; the *Path fields are the read-only
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
		policyFile:   policyFile,
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

// controlServerProbeTimeout bounds the startup dial in prepareSocket used to
// detect another live instance on a preexisting socket.
const controlServerProbeTimeout = 500 * time.Millisecond

// start begins serving on the control socket. net.Listen is the synchronous
// startup validation: once it returns, the socket exists and is bound. Serve
// runs in a single supervision goroutine; if it returns an unexpected error
// other than http.ErrServerClosed, onFatal is invoked so the daemon does not
// keep running with a dead control plane. onFatal is not invoked for a clean
// shutdown.
func (s *controlServer) start(onFatal func(error)) error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0755); err != nil {
		return fmt.Errorf("create control socket directory: %w", err)
	}
	if err := s.prepareSocket(); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on control socket %q: %w", s.socketPath, err)
	}
	// net.UnixListener removes the socket pathname when it is closed, so a
	// clean shutdown removes the file. A hard crash may leave a stale socket,
	// which prepareSocket detects on the next start via ECONNREFUSED.
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		listener.Close()
		return fmt.Errorf("control socket listener is not a Unix listener: %T", listener)
	}
	unixListener.SetUnlinkOnClose(true)
	s.listener = listener
	if err := os.Chmod(s.socketPath, 0660); err != nil {
		listener.Close()
		return fmt.Errorf("chmod control socket %q: %w", s.socketPath, err)
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		serveErr := s.server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			return
		}
		log.Printf("control server: %v", serveErr)
		// Surface the failure to the rest of the daemon so it does not keep
		// running while the control plane is dead.
		if onFatal != nil {
			onFatal(serveErr)
		}
	}()
	log.Printf("control API listening on %s", s.socketPath)
	return nil
}

// stop shuts the server down synchronously and closes the listener. With
// UnlinkOnClose set, the listener's Close removes the socket file, so the
// path is clean before stop returns. It is idempotent: a concurrent listener
// close (for example by the supervision goroutine after a fatal Serve error)
// is tolerated.
func (s *controlServer) stop() {
	if s.server == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.server.Shutdown(shutdownCtx)
	if s.listener != nil {
		// A concurrent close may already have closed the listener; that error
		// is expected and safe to ignore.
		_ = s.listener.Close()
		s.listener = nil
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

// managementInventory builds the disk policy view served to the WebUI and to
// `disks list`. It is read on demand from the current policy file and the
// Unraid inventory files with validateIncluded=false so that an incomplete or
// invalid disk still appears and can be excluded, matching the pre-control
// `disks list` behavior. It does not depend on the collector's thermal state.
func (s *controlServer) managementInventory() ([]diskPolicyRow, error) {
	policies, policyErr := readDiskPolicies(s.policyFile)
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
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request diskPolicySetRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := s.policies.Reset(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	requestDiskRefresh(s.refresh)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *controlServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	requestDiskRefresh(s.refresh)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
