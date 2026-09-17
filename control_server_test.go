// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type controlTestServer struct {
	socketPath string
	server     *controlServer
}

func startControlServerForTest(t *testing.T, environment *diskTestEnvironment, refresh chan struct{}, socketPath ...string) *controlTestServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sock")
	if len(socketPath) > 0 && socketPath[0] != "" {
		path = socketPath[0]
	}
	server := newControlServer(path, refresh, environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err != nil {
		t.Fatalf("start control server: %v", err)
	}
	result := &controlTestServer{socketPath: path, server: server}
	t.Cleanup(func() { result.stop(t) })
	return result
}

func (s *controlTestServer) stop(t *testing.T) {
	t.Helper()
	s.server.stop()
	waitForSocketGone(t, s.socketPath)
}

func waitForSocketGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s still present after shutdown", path)
}

func controlRequest(socketPath, method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequest(method, "http://uvss"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func TestControlServerLifecycleAndInventory(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))

	info, err := os.Lstat(server.socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0660 {
		t.Fatalf("control socket = %v, %v; want Unix socket mode 0660", info, err)
	}
	status, body, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/disks = %d, %v; body %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "serial" || rows[0].Device != "sda" || !rows[0].Selected || !rows[0].Eligible {
		t.Fatalf("inventory = %#v", rows)
	}

	server.stop(t)
}

func TestControlServerRefusesNonSocketPath(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	server := newControlServer(path, make(chan struct{}, 1), environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("start over regular file = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("existing file was removed: %v", err)
	}
}

func TestControlServerReclaimsStaleSocket(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := startControlServerForTest(t, environment, make(chan struct{}, 1), path)
	if status, _, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second); err != nil || status != http.StatusOK {
		t.Fatalf("GET after stale socket reclaim = %d, %v", status, err)
	}
}

func TestControlServerRefusesActiveInstance(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", path)
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
			_ = conn.Close()
		}
	}()
	server := newControlServer(path, make(chan struct{}, 1), environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	if err := server.start(); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("start over active socket = %v", err)
	}
}

func TestControlServerEmptyInventoryIsArray(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\nstatus=assigned_NP\nrotational=1\nspundown=0\n")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	status, body, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("empty inventory = %d, %v, %q", status, err, body)
	}
}

func TestControlServerIncompleteDiskVisibleForExclusion(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=../invalid\ntransport=ata\nrotational=1\nspundown=0\n")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	status, body, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET incomplete disk = %d, %v; %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "stable_id" || rows[0].Eligible || rows[0].ValidationError == "" {
		t.Fatalf("incomplete inventory = %#v", rows)
	}
}

func TestControlServerPolicyMutationIsImmediateAndRefreshes(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", false)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)

	status, body, err := controlRequest(server.socketPath, http.MethodPut, "/v1/disk-policy",
		diskPolicySetRequest{ID: "stable_id", Policy: diskPolicyExclude}, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("PUT policy = %d, %v; %q", status, err, body)
	}
	select {
	case <-refresh:
	default:
		t.Fatal("policy mutation did not request a refresh")
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || policies["stable_id"] != diskPolicyExclude {
		t.Fatalf("persisted policies = %#v, %v", policies, err)
	}
	status, body, err = controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after PUT = %d, %v; %q", status, err, body)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Policy != diskPolicyExclude || rows[0].Selected {
		t.Fatalf("inventory after PUT = %#v", rows)
	}

	status, body, err = controlRequest(server.socketPath, http.MethodDelete, "/v1/disk-policies", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("DELETE policies = %d, %v; %q", status, err, body)
	}
	if _, err := os.Stat(environment.paths.policyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("policy file after reset = %v; want absent", err)
	}
}

func TestControlServerInvalidPolicyFileCanBeReset(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	environment.write(t, environment.paths.policyFile, `{"serial":"bogus"}`)
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))

	status, _, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusUnprocessableEntity {
		t.Fatalf("GET invalid policies = %d, %v; want 422", status, err)
	}
	status, _, err = controlRequest(server.socketPath, http.MethodDelete, "/v1/disk-policies", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("DELETE invalid policies = %d, %v", status, err)
	}
	status, _, err = controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET after reset = %d, %v", status, err)
	}
}

func TestControlServerRejectsInvalidMutationBodies(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", server.socketPath)
	}}
	client := &http.Client{Transport: transport, Timeout: time.Second}

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "truncated", body: `{"id":"serial"`},
		{name: "unknown field", body: `{"id":"serial","policy":"include","extra":true}`},
		{name: "second value", body: `{"id":"serial","policy":"include"}{}`},
		{name: "oversized", body: strings.Repeat("x", int(controlRequestBodyLimit)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPut, "http://uvss/v1/disk-policy", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", response.StatusCode)
			}
		})
	}
}

func TestControlServerMethodRouting(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/v1/disks"},
		{http.MethodGet, "/v1/disk-policy"},
		{http.MethodPost, "/v1/disk-policies"},
	} {
		status, _, err := controlRequest(server.socketPath, test.method, test.path, nil, time.Second)
		if err != nil || status != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, %v; want 405", test.method, test.path, status, err)
		}
	}
}

func TestControlServerConcurrentSetsNoLostUpdate(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("disk-%d", i)
			status, body, err := controlRequest(server.socketPath, http.MethodPut, "/v1/disk-policy",
				diskPolicySetRequest{ID: id, Policy: diskPolicyInclude}, 3*time.Second)
			if err != nil {
				errs <- err
				return
			}
			if status != http.StatusOK {
				errs <- fmt.Errorf("%s: HTTP %d: %s", id, status, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || len(policies) != count {
		t.Fatalf("policies = %d entries, %v; want %d", len(policies), err, count)
	}
}

func TestControlServerShutdownWaitsForMutation(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	path := filepath.Join(t.TempDir(), "control.sock")
	server := newControlServer(path, make(chan struct{}, 1), environment.paths.policyFile,
		environment.paths.disksINI, environment.paths.devsINI, environment.paths.sysBlockRoot)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	mux := http.NewServeMux()
	server.registerRoutes(mux)
	started := make(chan struct{})
	release := make(chan struct{})
	server.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		mux.ServeHTTP(w, r)
	})}
	server.done = make(chan struct{})
	go func() {
		defer close(server.done)
		_ = server.server.Serve(listener)
	}()

	requestDone := make(chan error, 1)
	go func() {
		status, _, err := controlRequest(path, http.MethodPut, "/v1/disk-policy",
			diskPolicySetRequest{ID: "serial", Policy: diskPolicyInclude}, 2*time.Second)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("status = %d", status)
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("mutation did not reach server")
	}
	stopDone := make(chan struct{})
	go func() { server.stop(); close(stopDone) }()
	select {
	case <-stopDone:
		t.Fatal("shutdown returned while request was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("mutation during shutdown: %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	waitForSocketGone(t, path)
}

func TestControlServerRequestAfterShutdownFails(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	server := startControlServerForTest(t, environment, make(chan struct{}, 1))
	server.stop(t)
	if _, _, err := controlRequest(server.socketPath, http.MethodGet, "/v1/disks", nil, 200*time.Millisecond); err == nil {
		t.Fatal("request after shutdown succeeded")
	}
}
