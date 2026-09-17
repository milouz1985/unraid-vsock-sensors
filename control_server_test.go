// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// controlTestServer couples a running control server with its socket path so
// assertions can drive the API and clean the socket up afterwards.
type controlTestServer struct {
	socketPath string
	server     *controlServer
}

// startControlServerForTest starts a control server bound to a temp socket.
// socketPath may be empty to let the helper pick a fresh temp path; a non-empty
// value is used verbatim (for example to test stale-socket reclaim). The
// refresh channel is handed to the server constructor (write-only), so the
// server can request collections; the inventory is read on demand by the
// server, so no thermal priming is needed for the management endpoints.
func startControlServerForTest(t *testing.T, environment *diskTestEnvironment, refresh chan struct{}, socketPath ...string) *controlTestServer {
	t.Helper()
	if len(socketPath) == 0 || socketPath[0] == "" {
		socketPath = []string{filepath.Join(t.TempDir(), "control.sock")}
	}
	server := newControlServer(socketPath[0], refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err != nil {
		t.Fatalf("start control server: %v", err)
	}
	testServer := &controlTestServer{
		socketPath: socketPath[0], server: server,
	}
	t.Cleanup(func() {
		testServer.stop(t)
	})
	return testServer
}

// stop shuts the control server down synchronously and waits for the socket
// file to disappear.
func (s *controlTestServer) stop(t *testing.T) {
	t.Helper()
	s.server.stop()
	waitForSocketGone(t, s.socketPath)
}

// waitForSocketGone waits for the daemon to remove its socket file on shutdown.
func waitForSocketGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s still present after shutdown", path)
}

func controlClientForTest(t *testing.T, socketPath string) *controlClient {
	t.Helper()
	return newControlClient(socketPath)
}

func TestControlServerLifecycleAndSocketCleanup(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)

	info, err := os.Lstat(server.socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket = %v, %v; want a Unix socket", info, err)
	}
	perm := info.Mode().Perm()
	if perm != 0660 {
		t.Fatalf("socket permission = %o, want 0660", perm)
	}

	// A GET must succeed and return the on-demand inventory.
	client := controlClientForTest(t, server.socketPath)
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/disks = %d, %v; body %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "serial" || rows[0].Device != "sda" || !rows[0].Included {
		t.Fatalf("inventory = %#v", rows)
	}

	// A synchronous stop removes the socket before returning.
	server.stop(t)
	if _, err := os.Lstat(server.socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present after synchronous stop: %v", err)
	}
}

func TestControlServerRefusesNonSocketPath(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(socketPath, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	server := newControlServer(socketPath, refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("start over a regular file = %v; want a refusal", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("existing regular file was removed: %v", err)
	}
}

func TestControlServerRemovesStaleSocket(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	// Simulate a crashed daemon leaving a socket file behind. A listener with
	// UnlinkOnClose disabled is created at the target path and then closed:
	// the process is unbound but the socket file remains, exactly like a crash.
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale socket missing: %v, %v", info, err)
	}

	startControlServerForTest(t, environment, refresh, socketPath)
	client := controlClientForTest(t, socketPath)
	if status, _, err := client.do(http.MethodGet, "/v1/disks", nil); err != nil || status != http.StatusOK {
		t.Fatalf("GET after stale socket removal = %d, %v", status, err)
	}
}

// TestControlServerPreservesActiveSocketOnProbeError ensures that a preexisting
// socket that a process answers on is never removed: the probe sees a live
// connection (not ECONNREFUSED) and prepareSocket refuses to start, leaving the
// socket in place. This is the "another instance is active" path and the
// complement of the stale-socket removal test.
func TestControlServerPreservesActiveSocketOnProbeError(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	server := newControlServer(socketPath, refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.prepareSocket(); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("prepareSocket over an active socket = %v; want a refusal", err)
	}
	// The active socket must still be present.
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("active socket was removed: %v", err)
	}
}

func TestControlServerRefusesActiveInstance(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	// A live listener answering on the socket represents another active daemon.
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n[]"))
			}(conn)
		}
	}()
	defer listener.Close()

	server := newControlServer(socketPath, refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("start over an active instance = %v; want a refusal", err)
	}
}

func TestControlServerZeroDisksReturnsEmptyList(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	// A single section in the "not populated" state is counted (so the
	// inventory is not "empty") but skipped, leaving a valid empty list.
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\nstatus=assigned_NP\nrotational=1\nspundown=0\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/disks (empty) = %d, %v; body %q", status, err, body)
	}
	// The raw body must be a JSON array, not the string "null".
	if len(body) == 0 || string(body) == "null\n" || strings.TrimSpace(string(body)) == "null" {
		t.Fatalf("GET /v1/disks (empty) body = %q; want a JSON array", body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode empty inventory: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty inventory = %#v; want no rows", rows)
	}
}

// TestControlServerIncompleteDiskVisibleAndExcludable is the end-to-end
// regression for GET /v1/disks: a disk whose device cannot be resolved must
// still be listed (so the WebUI can exclude it) rather than hidden by the
// daemon's strict validation.
func TestControlServerIncompleteDiskVisibleAndExcludable(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=../invalid\ntransport=ata\nrotational=1\nspundown=0\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	// The incomplete disk is visible with an empty device and is includable.
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/disks (incomplete) = %d, %v; body %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode incomplete inventory: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "stable_id" || rows[0].Device != "" || !rows[0].Included {
		t.Fatalf("incomplete row = %#v; want stable_id, empty device, included", rows)
	}

	// Excluding the disk through the control API must succeed and persist. The
	// WebUI relies on being able to reach this disk to exclude it; the strict
	// collector-side validation of the excluded disk is covered separately.
	if status, _, err := client.doWithTimeout(http.MethodPut, "/v1/disk-policy",
		diskPolicySetRequest{ID: "stable_id", Policy: diskPolicyExclude}, controlClientMutationTimeout); err != nil || status != http.StatusOK {
		t.Fatalf("exclude incomplete disk = %d, %v", status, err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || policies["stable_id"] != diskPolicyExclude {
		t.Fatalf("persisted exclusion = %#v, %v; want exclude", policies, err)
	}
	// After exclusion the GET must still show the disk, now marked excluded.
	status, body, err = client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after exclude = %d, %v; body %q", status, err, body)
	}
	var rowsAfter []diskPolicyRow
	if err := json.Unmarshal(body, &rowsAfter); err != nil {
		t.Fatalf("decode inventory after exclude: %v", err)
	}
	if len(rowsAfter) != 1 || rowsAfter[0].ID != "stable_id" || rowsAfter[0].Policy != diskPolicyExclude || rowsAfter[0].Included {
		t.Fatalf("row after exclude = %#v; want stable_id, exclude, not included", rowsAfter)
	}
}

// TestControlServerPutThenGetImmediate verifies that a policy mutation is
// reflected by the very next GET without any manual refresh: the server reads
// the persisted policy file on demand.
func TestControlServerPutThenGetImmediate(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	if status, _, err := client.doWithTimeout(http.MethodPut, "/v1/disk-policy",
		diskPolicySetRequest{ID: "serial", Policy: diskPolicyInclude}, controlClientMutationTimeout); err != nil || status != http.StatusOK {
		t.Fatalf("PUT include = %d, %v", status, err)
	}
	// Immediately, without any refreshNow/refresh, the policy must be included.
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after PUT = %d, %v; body %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode inventory after PUT: %v", err)
	}
	if len(rows) != 1 || rows[0].Policy != diskPolicyInclude {
		t.Fatalf("inventory after immediate PUT = %#v; want policy include", rows)
	}
}

func TestControlServerSetPolicyPersistsAndRefreshes(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	if status, _, err := client.doWithTimeout(http.MethodPut, "/v1/disk-policy", diskPolicySetRequest{ID: "serial", Policy: diskPolicyInclude}, controlClientMutationTimeout); err != nil || status != http.StatusOK {
		t.Fatalf("PUT include = %d, %v", status, err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || policies["serial"] != diskPolicyInclude {
		t.Fatalf("persisted policies = %#v, %v", policies, err)
	}
	if len(refresh) != 1 {
		t.Fatalf("refresh channel = %d after set, want 1", len(refresh))
	}
	<-refresh

	// Reset must remove the override and refresh.
	if status, _, err := client.doWithTimeout(http.MethodDelete, "/v1/disk-policies", nil, controlClientMutationTimeout); err != nil || status != http.StatusOK {
		t.Fatalf("DELETE = %d, %v", status, err)
	}
	if _, err := os.Stat(environment.paths.policyFile); !os.IsNotExist(err) {
		t.Fatalf("policy file after reset: %v; want absent", err)
	}
	if len(refresh) != 1 {
		t.Fatalf("refresh channel = %d after reset, want 1", len(refresh))
	}
}

func TestControlServerSetPolicyValidation(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	tests := []struct {
		name             string
		body             string
		status           int
		wantErrorContain string
	}{
		{name: "invalid policy", body: `{"id":"serial","policy":"bogus"}`, status: http.StatusBadRequest},
		{name: "truncated JSON", body: `{"id":"serial",`, status: http.StatusBadRequest, wantErrorContain: "invalid JSON body"},
		{name: "missing id", body: `{"policy":"include"}`, status: http.StatusBadRequest},
		{name: "empty body", body: ``, status: http.StatusBadRequest, wantErrorContain: "invalid JSON body"},
		{name: "unknown field", body: `{"id":"serial","policy":"include","extra":true}`, status: http.StatusBadRequest, wantErrorContain: `unknown field "extra"`},
		{name: "multiple JSON values", body: `{"id":"serial","policy":"include"} {}`, status: http.StatusBadRequest, wantErrorContain: "multiple JSON values are not allowed"},
		{name: "body too large", body: `{"id":"` + strings.Repeat("a", int(controlRequestBodyLimit)) + `","policy":"include"}`, status: http.StatusBadRequest, wantErrorContain: "request body too large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPut, "http://uvss/v1/disk-policy", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if test.wantErrorContain != "" {
				var payload struct {
					Error string `json:"error"`
				}
				if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if !strings.Contains(payload.Error, test.wantErrorContain) {
					t.Fatalf("error = %q; want substring %q", payload.Error, test.wantErrorContain)
				}
			}
			// No refresh must be requested for a rejected mutation.
			select {
			case <-refresh:
				t.Fatal("rejected mutation requested a refresh")
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestControlServerRefresh(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	if status, _, err := client.do(http.MethodPost, "/v1/refresh", nil); err != nil || status != http.StatusOK {
		t.Fatalf("POST /v1/refresh = %d, %v", status, err)
	}
	if len(refresh) != 1 {
		t.Fatalf("refresh channel = %d after refresh, want 1", len(refresh))
	}
	<-refresh
}

func TestControlServerMethodNotAllowed(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/disks"},
		{method: http.MethodPost, path: "/v1/policies"},
		{method: http.MethodPost, path: "/v1/disk-policy"},
		{method: http.MethodGet, path: "/v1/disk-policies"},
		{method: http.MethodGet, path: "/v1/refresh"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			status, _, err := client.do(tt.method, tt.path, nil)
			if err != nil || status != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, %v; want 405", tt.method, tt.path, status, err)
			}
		})
	}
}

func TestControlServerConcurrentSetsNoLostUpdate(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	var wg sync.WaitGroup
	const workers = 16
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("disk%d", i)
			for range 5 {
				status, _, err := client.doWithTimeout(http.MethodPut, "/v1/disk-policy", diskPolicySetRequest{ID: id, Policy: diskPolicyInclude}, controlClientMutationTimeout)
				if err != nil || status != http.StatusOK {
					t.Errorf("set %s = %d, %v", id, status, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != workers {
		t.Fatalf("concurrent sets = %d policies, want %d", len(policies), workers)
	}
}

func TestControlClientDaemonNotRunning(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "absent.sock")
	client := newControlClient(socketPath)
	_, _, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err == nil || !errors.Is(err, errDaemonNotRunning) {
		t.Fatalf("error = %v; want errDaemonNotRunning", err)
	}
}

func TestControlClientMutationTimeoutReportsUncertainOutcome(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	client := newControlClient(socketPath)
	_, _, err = client.doMutationWithTimeout(http.MethodPut, "/v1/disk-policy",
		diskPolicySetRequest{ID: "serial", Policy: diskPolicyInclude}, 100*time.Millisecond)
	if err == nil || !errors.Is(err, errControlTimeout) {
		t.Fatalf("mutation error = %v; want errControlTimeout", err)
	}
	if !strings.Contains(err.Error(), "the mutation may still have been applied") {
		t.Fatalf("mutation timeout = %q; missing uncertain-outcome warning", err)
	}

	_, _, err = client.doWithTimeout(http.MethodGet, "/v1/disks", nil, 100*time.Millisecond)
	if err == nil || !errors.Is(err, errControlTimeout) {
		t.Fatalf("runtime error = %v; want errControlTimeout", err)
	}
	if strings.Contains(err.Error(), "mutation may still have been applied") {
		t.Fatalf("runtime timeout unexpectedly carries mutation warning: %q", err)
	}
}

func TestControlTimeoutBudgets(t *testing.T) {
	if controlClientMutationTimeout != 5*time.Second {
		t.Fatalf("mutation timeout = %s; want 5s", controlClientMutationTimeout)
	}
	if controlServerShutdownTimeout <= controlClientMutationTimeout {
		t.Fatalf("shutdown timeout %s must exceed mutation timeout %s", controlServerShutdownTimeout, controlClientMutationTimeout)
	}
}

// TestControlServerShutdownWaitsForMutation verifies that graceful shutdown
// does not abort an accepted policy mutation. The wrapper deliberately blocks
// the request before the real handler runs, then stop is called while the
// request is in flight. Both PUT and DELETE must be allowed to complete before
// stop returns.
func TestControlServerShutdownWaitsForMutation(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "set policy",
			method: http.MethodPut,
			path:   "/v1/disk-policy",
			body:   diskPolicySetRequest{ID: "serial", Policy: diskPolicyInclude},
		},
		{
			name:   "reset policies",
			method: http.MethodDelete,
			path:   "/v1/disk-policies",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := newDiskTestEnvironment(t, "30")
			refresh := make(chan struct{}, 1)
			socketPath := filepath.Join(t.TempDir(), "control.sock")
			server := newControlServer(socketPath, refresh, environment.paths.policyFile,
				environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)

			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			unixListener, ok := listener.(*net.UnixListener)
			if !ok {
				listener.Close()
				t.Fatalf("listener = %T; want *net.UnixListener", listener)
			}
			unixListener.SetUnlinkOnClose(true)
			server.listener = listener

			mux := http.NewServeMux()
			server.registerRoutes(mux)
			requestStarted := make(chan struct{})
			releaseRequest := make(chan struct{})
			var releaseOnce sync.Once
			var stopOnce sync.Once
			stopServer := func() { stopOnce.Do(server.stop) }
			server.server = &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == tt.method && r.URL.Path == tt.path {
						close(requestStarted)
						<-releaseRequest
					}
					mux.ServeHTTP(w, r)
				}),
			}
			go func() {
				_ = server.server.Serve(listener)
			}()
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseRequest) })
				stopServer()
			})

			client := controlClientForTest(t, socketPath)
			requestDone := make(chan error, 1)
			go func() {
				status, _, err := client.doWithTimeout(tt.method, tt.path, tt.body, controlClientMutationTimeout)
				if err == nil && status != http.StatusOK {
					err = fmt.Errorf("status = %d; want 200", status)
				}
				requestDone <- err
			}()

			select {
			case <-requestStarted:
			case <-time.After(time.Second):
				t.Fatal("mutation request did not reach the server")
			}

			stopDone := make(chan struct{})
			go func() {
				stopServer()
				close(stopDone)
			}()

			select {
			case <-stopDone:
				t.Fatal("shutdown returned while a mutation was still in flight")
			case <-time.After(100 * time.Millisecond):
			}

			releaseOnce.Do(func() { close(releaseRequest) })
			select {
			case err := <-requestDone:
				if err != nil {
					t.Fatalf("mutation failed during shutdown: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("mutation did not complete after release")
			}
			select {
			case <-stopDone:
			case <-time.After(time.Second):
				t.Fatal("shutdown did not complete after mutation finished")
			}
			waitForSocketGone(t, socketPath)
		})
	}
}

func TestControlServerRequestAfterShutdownFails(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	server.stop(t)
	// A request after shutdown must fail promptly rather than hang.
	if _, _, err := client.do(http.MethodGet, "/v1/disks", nil); err == nil {
		t.Fatal("request after shutdown succeeded")
	}
}

// TestControlServerInvalidPolicyFileDetected verifies that a corrupted policy
// file surfaces as a 422 on /v1/disks (so the WebUI can offer a reset) and as
// a failure on /v1/policies.
func TestControlServerInvalidPolicyFileDetected(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	environment.write(t, environment.paths.policyFile, `{ "serial": "bogus" }`)
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil {
		t.Fatalf("GET /v1/disks = %v", err)
	}
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("GET /v1/disks with invalid policies = %d; want 422, body %q", status, body)
	}
	var policyErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &policyErr); err != nil || policyErr.Error == "" {
		t.Fatalf("invalid policy error payload = %q, %v", body, err)
	}

	status, _, err = client.do(http.MethodGet, "/v1/policies", nil)
	if err != nil {
		t.Fatalf("GET /v1/policies = %v", err)
	}
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("GET /v1/policies with invalid policies = %d; want 422", status)
	}
}

// TestControlServerResetInvalidThenGetImmediate verifies that a reset of a
// corrupted policy file makes the next GET succeed immediately (no manual
// refresh) and reports a clean policy state.
func TestControlServerResetInvalidThenGetImmediate(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	environment.write(t, environment.paths.policyFile, `{ "serial": "bogus" }`)
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	if status, _, err := client.doWithTimeout(http.MethodDelete, "/v1/disk-policies", nil, controlClientMutationTimeout); err != nil || status != http.StatusOK {
		t.Fatalf("DELETE = %d, %v", status, err)
	}
	// Immediately, without any manual refresh, the inventory must be valid
	// again and the policy check must pass.
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after reset = %d, %v; body %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode inventory after reset: %v", err)
	}
	if len(rows) != 1 || rows[0].Policy != diskPolicyAuto {
		t.Fatalf("inventory after reset = %#v; want a single auto disk", rows)
	}
	if status, _, err := client.do(http.MethodGet, "/v1/policies", nil); err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/policies after reset = %d, %v; want 200", status, err)
	}
}

// TestControlServerEarlyRefreshNotLost verifies that a refresh requested before
// the collection loop has started is not lost: the buffered channel preserves
// it (len==1) so the first loop iteration can consume it. A second concurrent
// request must coalesce (still len==1) rather than block or drop the first.
func TestControlServerEarlyRefreshNotLost(t *testing.T) {
	refresh := make(chan struct{}, 1)
	// A refresh requested before the collection loop runs must be retained.
	requestDiskRefresh(refresh)
	if len(refresh) != 1 {
		t.Fatalf("early refresh not retained = %d; want 1", len(refresh))
	}
	// A second concurrent request must coalesce, not block or overflow.
	requestDiskRefresh(refresh)
	if len(refresh) != 1 {
		t.Fatalf("coalesced refresh = %d; want 1", len(refresh))
	}
	// The single retained signal is delivered to the consumer (the loop).
	select {
	case <-refresh:
	case <-time.After(time.Second):
		t.Fatal("retained refresh was not delivered")
	}
	if len(refresh) != 0 {
		t.Fatalf("refresh not consumed = %d; want 0", len(refresh))
	}
}

// TestControlServerServeErrorRecovers verifies that losing the active listener
// does not terminate control supervision. The socket is recreated and becomes
// usable again without any daemon-level fatal callback.
func TestControlServerServeErrorRecovers(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	server.server.mu.Lock()
	listener := server.server.listener
	done := server.server.done
	server.server.mu.Unlock()
	if listener == nil || done == nil {
		t.Fatal("control server has no active listener/supervision channel")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("force listener failure: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _, err := client.do(http.MethodGet, "/v1/disks", nil)
		if err == nil && status == http.StatusOK {
			select {
			case <-done:
				t.Fatal("control supervision stopped after listener recovery")
			default:
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("control API did not recover after its listener was closed")
}

// TestControlServerRecoverySurvivesTemporaryBindFailure verifies that a
// recovery error is retried rather than turning a control-plane problem into a
// daemon shutdown. A regular file temporarily occupies the socket path, then
// removal of that obstacle lets the API recover.
func TestControlServerRecoverySurvivesTemporaryBindFailure(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	client := controlClientForTest(t, server.socketPath)

	server.server.mu.Lock()
	listener := server.server.listener
	done := server.server.done
	server.server.mu.Unlock()
	if listener == nil || done == nil {
		t.Fatal("control server has no active listener/supervision channel")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("force listener failure: %v", err)
	}
	waitForSocketGone(t, server.socketPath)
	if err := os.WriteFile(server.socketPath, []byte("temporary obstacle"), 0600); err != nil {
		t.Fatalf("occupy socket path: %v", err)
	}

	// Let at least one recovery attempt hit the temporary regular file.
	time.Sleep(controlServerRetryInitial + 100*time.Millisecond)
	select {
	case <-done:
		t.Fatal("control supervision stopped after a recoverable bind failure")
	default:
	}
	if err := os.Remove(server.socketPath); err != nil {
		t.Fatalf("remove temporary obstacle: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _, err := client.do(http.MethodGet, "/v1/disks", nil)
		if err == nil && status == http.StatusOK {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("control API did not recover after the bind obstacle was removed")
}

// TestControlServerStaleSocketCrashRecovery simulates a crash leaving a socket
// file, then verifies a new instance reclaims the path and serves.
func TestControlServerStaleSocketCrashRecovery(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "control.sock")

	// Simulate a crash: create a residual socket file at the path with no
	// bound process (UnlinkOnClose disabled, then close).
	crashListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if unixListener, ok := crashListener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := crashListener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("crash left no socket file: %v", err)
	}

	// A second instance must reclaim the path and serve.
	refresh := make(chan struct{}, 1)
	server := newControlServer(socketPath, refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	t.Cleanup(server.stop)
	if err := server.start(); err != nil {
		t.Fatalf("second instance start: %v", err)
	}
	client := controlClientForTest(t, socketPath)
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after crash recovery = %d, %v; body %q", status, err, body)
	}
}
