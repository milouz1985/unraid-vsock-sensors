package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
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
		handle(server, path, newHBACollector(time.Minute, hbaModeEnabled))
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

func TestDiskErrorDoesNotBlockHBASelector(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{Name: "hba0", Temp: 46}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeHBA, "hba0"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "46\n" {
		t.Fatalf("got %q, want HBA temperature", got)
	}
}

func TestDiskSelectorStillReturnsDiskError(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{Name: "hba0", Temp: 46}}}
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
