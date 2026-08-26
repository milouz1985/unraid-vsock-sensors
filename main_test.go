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
		handle(server, path, newHBACollector(time.Minute))
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
	if err := writeResponse(&out, r, "hba0", false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "46\n" {
		t.Fatalf("got %q, want HBA temperature", got)
	}
}

func TestDiskSelectorStillReturnsDiskError(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{Name: "hba0", Temp: 46}}}
	if err := writeResponse(&bytes.Buffer{}, r, "hdd", false); err == nil || err.Error() != r.Error {
		t.Fatalf("got %v, want disk error", err)
	}
}
