package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestServerResponseIncludesVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, []byte("[disk1]\nid=serial\ndevice=sdb\ntemp=35\nrotational=1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		handle(server, newDiskReader(path, time.Minute), newHBACollector(time.Minute, hbaModeEnabled))
		close(done)
	}()
	if _, err := io.WriteString(client, "GET\n"); err != nil {
		t.Fatal(err)
	}
	var response sensors.Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	client.Close()
	<-done

	if response.Version != version {
		t.Fatalf("got version %q, want %q", response.Version, version)
	}
}

func TestServerResponseReportsDisabledHBACollection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, []byte("[disk1]\nid=serial\ndevice=sdb\ntemp=35\nrotational=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		handle(server, newDiskReader(path, time.Minute), newHBACollector(time.Minute, hbaModeDisabled))
		close(done)
	}()
	if _, err := io.WriteString(client, "GET\n"); err != nil {
		t.Fatal(err)
	}
	var response sensors.Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	client.Close()
	<-done
	if !response.HBADisabled {
		t.Fatal("disabled HBA collection was not reported")
	}
}

func TestHandleRejectsInvalidRequest(t *testing.T) {
	for name, request := range map[string]string{
		"invalid command": "POST\n",
		// The first 1024 bytes trim to GET. Reading only 1024 bytes would
		// therefore accept this request without noticing the final byte.
		"oversized": "GET" + strings.Repeat(" ", maxRequestSize-len("GET")) + "X",
	} {
		t.Run(name, func(t *testing.T) {
			server, client := net.Pipe()
			disks := newDiskReader(filepath.Join(t.TempDir(), "missing.ini"), time.Minute)
			done := make(chan struct{})
			go func() {
				handle(server, disks, newHBACollector(time.Minute, hbaModeEnabled))
				close(done)
			}()

			writeDone := make(chan error, 1)
			go func() {
				_, err := io.WriteString(client, request)
				writeDone <- err
			}()
			response, err := io.ReadAll(client)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			<-done
			if len(response) != 0 {
				t.Fatalf("invalid request returned %q", response)
			}
		})
	}
}

func TestHandleReadTimeout(t *testing.T) {
	server, client := net.Pipe()
	disks := newDiskReader(filepath.Join(t.TempDir(), "missing.ini"), time.Minute)
	done := make(chan struct{})
	go func() {
		handleWithTimeout(server, disks, newHBACollector(time.Minute, hbaModeEnabled), 20*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("silent client was not disconnected after the read timeout")
	}
	client.Close()
}

func TestHandleStopsWhenSettingDeadlineFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		failRead bool
		request  string
	}{
		{name: "read deadline", failRead: true},
		{name: "write deadline", request: "GET\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			conn := &deadlineFailConn{Conn: server, failRead: test.failRead}
			disks := newDiskReader(filepath.Join(t.TempDir(), "missing.ini"), time.Minute)
			done := make(chan struct{})
			go func() {
				handle(conn, disks, newHBACollector(time.Minute, hbaModeEnabled))
				close(done)
			}()

			if test.request != "" {
				if _, err := io.WriteString(client, test.request); err != nil {
					t.Fatal(err)
				}
			}
			response, err := io.ReadAll(client)
			if err != nil {
				t.Fatal(err)
			}
			client.Close()
			<-done
			if len(response) != 0 {
				t.Fatalf("deadline failure returned %q", response)
			}
			if conn.readCalls != 1 {
				t.Fatalf("SetReadDeadline called %d times, want 1", conn.readCalls)
			}
			wantWriteCalls := 1
			if test.failRead {
				wantWriteCalls = 0
			}
			if conn.writeCalls != wantWriteCalls {
				t.Fatalf("SetWriteDeadline called %d times, want %d", conn.writeCalls, wantWriteCalls)
			}
		})
	}
}

type deadlineFailConn struct {
	net.Conn
	failRead   bool
	readCalls  int
	writeCalls int
}

func (conn *deadlineFailConn) SetReadDeadline(deadline time.Time) error {
	conn.readCalls++
	if conn.failRead {
		return errors.New("deadline failed")
	}
	return conn.Conn.SetReadDeadline(deadline)
}

func (conn *deadlineFailConn) SetWriteDeadline(time.Time) error {
	conn.writeCalls++
	return errors.New("deadline failed")
}

func TestDiskErrorDoesNotBlockHBASelector(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 46}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeHBA, "sas:1234"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "46\n" {
		t.Fatalf("got %q, want HBA temperature", got)
	}
}

func TestDiskSelectorStillReturnsDiskError(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 46}}}
	if err := writeResponse(&bytes.Buffer{}, r, sensorTypeDisk, "hdd"); err == nil || err.Error() != r.Error {
		t.Fatalf("got %v, want disk error", err)
	}
}

func TestDiskNamedLikeHBAIsSelectedAsDisk(t *testing.T) {
	r := sensors.Response{Disks: []sensors.Disk{{Name: "hba1", Temp: 38}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeDisk, "hba1"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "38\n" {
		t.Fatalf("got %q, want disk temperature", got)
	}
}

func TestDiskMaximumFailsSafeForUnavailableDisk(t *testing.T) {
	r := sensors.Response{Disks: []sensors.Disk{
		{Name: "disk1", Rotational: true, Temp: 38},
		{Name: "disk2", Rotational: true, Unavailable: true},
	}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeDisk, "hdd"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "100\n" {
		t.Fatalf("got %q, want group failsafe", got)
	}
}

func TestWriteResponseSelectors(t *testing.T) {
	response := sensors.Response{
		Disks: []sensors.Disk{
			{Name: "disk1", Device: "sdb", Rotational: true, Temp: 35},
			{Name: "disk2", Device: "sdc", Rotational: true, Temp: 40},
			{Name: "cache", Device: "sdd", Transport: "ata", Temp: 44.5},
			{Name: "fast", Device: "nvme0n1", Transport: "nvme", Temp: 48},
		},
		HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 49}, {ID: "pci:0000:06:10.0", Temp: 55}},
	}
	tests := []struct {
		name     string
		kind     sensorType
		selector string
		want     string
	}{
		{name: "HDD maximum", kind: sensorTypeDisk, selector: "hdd", want: "40\n"},
		{name: "SSD maximum", kind: sensorTypeDisk, selector: "ssd", want: "44.5\n"},
		{name: "NVMe maximum", kind: sensorTypeDisk, selector: "nvme", want: "48\n"},
		{name: "all disks", kind: sensorTypeDisk, selector: "all", want: "48\n"},
		{name: "disk name case insensitive", kind: sensorTypeDisk, selector: "DISK1", want: "35\n"},
		{name: "device case insensitive", kind: sensorTypeDisk, selector: "SDD", want: "44.5\n"},
		{name: "all HBAs", kind: sensorTypeHBA, selector: "all", want: "55\n"},
		{name: "HBA ID case insensitive", kind: sensorTypeHBA, selector: "SAS:1234", want: "49\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeResponse(&out, response, test.kind, test.selector); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestWriteResponseRejectsUnavailableSelector(t *testing.T) {
	for _, test := range []struct {
		name     string
		response sensors.Response
		kind     sensorType
		selector string
		want     string
	}{
		{name: "unknown disk", kind: sensorTypeDisk, selector: "missing", want: `no available temperature for selector "missing"`},
		{name: "unknown HBA", kind: sensorTypeHBA, selector: "sas:missing", want: `no available temperature for selector "sas:missing"`},
		{name: "HBA collection error", response: sensors.Response{HBAError: "storcli failed"}, kind: sensorTypeHBA, selector: "all", want: "HBA temperature unavailable: storcli failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := writeResponse(&bytes.Buffer{}, test.response, test.kind, test.selector)
			if err == nil || err.Error() != test.want {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestJSONResponsePreservesPartialErrors(t *testing.T) {
	want := sensors.Response{
		Version:     "test-version",
		Timestamp:   time.Date(2026, time.September, 1, 12, 30, 0, 0, time.UTC),
		Disks:       []sensors.Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
		HBAs:        []sensors.HBA{{ID: "sas:1234", Temp: 49}},
		HBADisabled: true,
		HBAError:    "storcli failed",
		Error:       "disks.ini incomplete",
	}
	var out bytes.Buffer
	if err := writeJSONResponse(&out, want); err != nil {
		t.Fatal(err)
	}

	var got sensors.Response
	if err := json.NewDecoder(&out).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || !got.Timestamp.Equal(want.Timestamp) || got.HBADisabled != want.HBADisabled || got.Error != want.Error || got.HBAError != want.HBAError {
		t.Fatalf("response metadata or errors changed: %#v", got)
	}
	if len(got.Disks) != 1 || got.Disks[0].Temp != 35 || len(got.HBAs) != 1 || got.HBAs[0].Temp != 49 {
		t.Fatalf("partial readings were lost: %#v", got)
	}
}

func TestWriteResponseRejectsUnknownSensorType(t *testing.T) {
	err := writeResponse(&bytes.Buffer{}, sensors.Response{}, sensorType("fan"), "all")
	if err == nil || err.Error() != `unknown sensor type "fan" (expected disk or hba)` {
		t.Fatalf("got %v", err)
	}
}

func TestGetRequiresExplicitSensorTypeAndSelector(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing both", want: "a sensor type and selector are required (for example: disk hdd or hba all)"},
		{name: "missing type", args: []string{"hdd"}, want: "a sensor type and selector are required (for example: disk hdd or hba all)"},
		{name: "unknown type", args: []string{"fan", "all"}, want: `unknown sensor type "fan" (expected disk or hba)`},
		{name: "JSON with arguments", args: []string{"--json", "disk", "all"}, want: "--json does not accept a sensor type or selector"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := get(test.args)
			if err == nil || err.Error() != test.want {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
