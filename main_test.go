package main

import (
	"bytes"
	"context"
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

func TestReadAndSelect(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	data := strings.ReplaceAll(strings.TrimSpace(`
		["disk1"]
		name="disk1"
		id="WDC_disk1_serial"
		device="sdb"
		temp="35"
		rotational="1"
		transport="ata"

		["fast"]
		id="NVME_fast_serial"
		device="nvme0n1"
		temp="48"
		rotational="0"
		transport="nvme"

		["disk2"]
		id="WDC_disk2_serial"
		device="sdc"
		temp="*"
		rotational="1"
		spundown="1"

		["disk13"]
		device=""
		status="DISK_NP"
		temp="*"
		spundown="0"

		["flash"]
		device="sdi"
		transport="usb"
		rotational="1"
		temp="*"
		spundown="0"
	`), "\n\t\t", "\n")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	disks, err := readDisks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 3 {
		t.Fatalf("got %d disks", len(disks))
	}
	if disks[0].ID != "WDC_disk1_serial" {
		t.Fatalf("got disk ID %q", disks[0].ID)
	}
	if got := selectDisks(disks, "hdd"); len(got) != 2 || got[0].Temp != 35 || got[1].Temp != 0 {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "nvme"); len(got) != 1 || got[0].Temp != 48 {
		t.Fatalf("nvme: %#v", got)
	}
	if got := selectDisks(disks, "fast"); len(got) != 1 || got[0].Device != "nvme0n1" {
		t.Fatalf("name: %#v", got)
	}
}

func TestReadDisksRejectsInvalidTemperature(t *testing.T) {
	for _, temperature := range []string{"broken", "NaN", "+Inf", "-Inf"} {
		t.Run(temperature, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "disks.ini")
			data := "[disk1]\nid=serial\ndevice=sdb\ntemp=" + temperature + "\nrotational=1\n"
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDisks(p); err == nil {
				t.Fatal("invalid temperature should remain an error")
			}
		})
	}
}

func TestReadDisksRejectsMissingTemperatureOnActiveDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(p, []byte("[disk1]\nid=serial\ndevice=sdb\ntemp=*\nrotational=1\nspundown=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDisks(p); err == nil {
		t.Fatal("missing temperature on an active disk should remain an error")
	}
}

func TestJSONIncludesDiskError(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{Name: "hba0", Temp: 46}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, "", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"error":"disks.ini failed"`) {
		t.Fatalf("JSON response omitted the error: %s", out.String())
	}
}

func TestValidateVsockAddresses(t *testing.T) {
	for _, cid := range []uint{3, uint(maxVsockAddress)} {
		if err := validateVsockCID(cid); err != nil {
			t.Fatalf("valid CID %d rejected: %v", cid, err)
		}
	}
	for _, cid := range []uint{0, 2} {
		if err := validateVsockCID(cid); err == nil {
			t.Fatalf("invalid CID %d accepted", cid)
		}
	}
	for _, port := range []uint{1, uint(maxVsockAddress)} {
		if err := validateVsockPort(port); err != nil {
			t.Fatalf("valid port %d rejected: %v", port, err)
		}
	}
	if err := validateVsockPort(0); err == nil {
		t.Fatal("zero port accepted")
	}
	if ^uint(0) > uint(maxVsockAddress) {
		overflow := uint(maxVsockAddress + 1)
		if err := validateVsockCID(overflow); err == nil {
			t.Fatalf("reserved CID %d accepted", overflow)
		}
		if err := validateVsockPort(overflow); err == nil {
			t.Fatalf("reserved port %d accepted", overflow)
		}
	}
}

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

func TestHBACollectorReadDoesNotWaitForRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	collector := newHBACollector(time.Minute)
	collector.collect = func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{Name: "hba0", Temp: 42}}, nil
	}

	done := make(chan struct{})
	go func() {
		collector.refresh(context.Background())
		close(done)
	}()
	<-started

	readDone := make(chan struct{})
	go func() {
		collector.read()
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("read blocked while StorCLI refresh was running")
	}
	close(release)
	<-done
}

func TestHBACollectorFailurePolicy(t *testing.T) {
	collector := newHBACollector(time.Minute)
	setSuccessfulRefresh := func(temp float64) {
		collector.collect = func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{Name: "hba0", Temp: temp}}, nil
		}
		collector.refresh(context.Background())
	}
	setFailedRefresh := func() {
		collector.collect = func(context.Context) ([]sensors.HBA, error) {
			return nil, errors.New("storcli failed")
		}
		collector.refresh(context.Background())
	}
	checkTemp := func(want float64) {
		t.Helper()
		readings, _ := collector.read()
		if len(readings) != 1 || readings[0].Temp != want {
			t.Fatalf("got readings %#v, want temperature %v", readings, want)
		}
	}

	setSuccessfulRefresh(42)
	checkTemp(42)

	// Two transient failures keep the last successful value available.
	for range maxFailedRefreshes - 1 {
		setFailedRefresh()
		checkTemp(42)
	}

	// The third consecutive failure invalidates the stale value.
	setFailedRefresh()
	readings, err := collector.read()
	if len(readings) != 0 || err == nil {
		t.Fatalf("stale readings should be unavailable: %#v, %v", readings, err)
	}

	// A success restores service and implicitly resets the failure count: the
	// next isolated failure must keep the new value available.
	setSuccessfulRefresh(50)
	setFailedRefresh()
	checkTemp(50)

	readings, err = collector.read()
	if err == nil || err.Error() != "storcli failed" {
		t.Fatalf("refresh error was not exposed: %v", err)
	}
}

func TestParseStorCLI(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"CLI Version":"007.3404.0000.0000 April 18, 2025","Operating system":"Linux 6.18.38-Unraid","Controller":0,"Status":"Success","Description":"None"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}]}},
		{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}
	]}`)
	hbas, err := parseStorCLI(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(hbas) != 2 || hbas[0].Name != "hba0" || hbas[0].Temp != 49 || hbas[1].Temp != 60 {
		t.Fatalf("unexpected HBA readings: %#v", hbas)
	}
	if got := selectHBAs(hbas, "hba"); len(got) != 2 {
		t.Fatalf("hba selector: %#v", got)
	}
	if got := selectHBAs(hbas, "hba1"); len(got) != 1 || got[0].Temp != 60 {
		t.Fatalf("hba1 selector: %#v", got)
	}
}

func TestParseStorCLIRejectsUnexpectedOutput(t *testing.T) {
	for name, data := range map[string]string{
		"failed status": `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`,
		"NaN":           storCLIResponseWithTemperature("NaN"),
		"positive Inf":  storCLIResponseWithTemperature("+Inf"),
		"negative Inf":  storCLIResponseWithTemperature("-Inf"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStorCLI([]byte(data)); err == nil {
				t.Fatal("unexpected StorCLI output should be rejected")
			}
		})
	}
}

func storCLIResponseWithTemperature(temperature string) string {
	return `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"` + temperature + `"}]}}]}`
}
