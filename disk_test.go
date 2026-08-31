package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestDiskReaderAllowsSpinupGrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	write := func(temp, spundown string) {
		data := "[disk1]\nid=serial1\ndevice=sdb\ntemp=" + temp + "\nspundown=" + spundown + "\nrotational=1\n"
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(1000, 0)
	reader := newDiskReader(path, 35*time.Second)
	reader.now = func() time.Time { return now }

	write("35", "0")
	disks, err := reader.read()
	if err != nil || disks[0].Temp != 35 {
		t.Fatalf("initial reading = %#v, %v", disks, err)
	}
	write("*", "1")
	if disks, err = reader.read(); err != nil || disks[0].Temp != 0 || disks[0].Unavailable {
		t.Fatalf("standby reading = %#v, %v", disks, err)
	}
	write("*", "0")
	if disks, err = reader.read(); err != nil || disks[0].Temp != 35 || disks[0].Unavailable {
		t.Fatalf("spinup grace reading = %#v, %v", disks, err)
	}
	now = now.Add(35 * time.Second)
	if disks, err = reader.read(); err != nil || !disks[0].Unavailable {
		t.Fatalf("expired spinup reading = %#v, %v", disks, err)
	}
	write("40", "0")
	if disks, err = reader.read(); err != nil || disks[0].Temp != 40 || disks[0].Unavailable {
		t.Fatalf("recovered reading = %#v, %v", disks, err)
	}
}

func TestDiskReaderGraceWithoutPreviousTemperatureAvoidsFailsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, []byte("[disk1]\nid=serial1\ndevice=sdb\ntemp=*\nspundown=0\nrotational=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reader := newDiskReader(path, time.Minute)
	disks, err := reader.read()
	if err != nil || len(disks) != 1 || disks[0].Temp != 0 || disks[0].Unavailable {
		t.Fatalf("initial grace reading = %#v, %v", disks, err)
	}
	readings := makeDiskReadings(sensors.Response{Disks: disks})
	if len(readings) != 1 || readings[0].temperature == hwmonFailsafeTemp {
		t.Fatalf("initial grace triggered hwmon failsafe: %#v", readings)
	}
}

func TestDiskReaderPurgesStateForDisappearedDisks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	write := func(data string) {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const active = "[disk1]\nid=serial1\ndevice=sdb\ntemp=35\nspundown=0\nrotational=1\n"
	const pending = "[disk1]\nid=serial1\ndevice=sdb\ntemp=*\nspundown=0\nrotational=1\n"

	now := time.Unix(1000, 0)
	reader := newDiskReader(path, time.Minute)
	reader.now = func() time.Time { return now }
	write(active)
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	write(pending)
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}

	write("[disk1]\nstatus=DISK_NP\n")
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	if len(reader.lastValid) != 0 || len(reader.pendingSince) != 0 {
		t.Fatalf("state was not purged: lastValid=%v pendingSince=%v", reader.lastValid, reader.pendingSince)
	}

	now = now.Add(2 * time.Minute)
	write(pending)
	disks, err := reader.read()
	if err != nil || len(disks) != 1 || disks[0].Temp != 0 || disks[0].Unavailable {
		t.Fatalf("reappeared disk reused stale state: disks=%#v err=%v", disks, err)
	}
}

func TestDiskReaderDeduplicatesRotationalWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	write := func(rotational string) {
		data := "[disk1]\nid=serial1\ndevice=sdb\ntemp=35\nrotational=" + rotational + "\n"
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	reader := newDiskReader(path, time.Minute)

	write("broken")
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "warning:"); got != 1 {
		t.Fatalf("repeated invalid value logged %d warnings: %q", got, output.String())
	}

	write("different")
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "warning:"); got != 2 {
		t.Fatalf("changed invalid value logged %d warnings: %q", got, output.String())
	}

	write("1")
	if _, err := reader.read(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "rotational value recovered"); got != 1 {
		t.Fatalf("recovery logged %d times: %q", got, output.String())
	}
}

func TestDiskSpinupGraceUsesPollAttributesWithoutCap(t *testing.T) {
	for name, value := range map[string]struct {
		config   string
		wantPoll time.Duration
		want     time.Duration
	}{
		"poll plus margin": {config: "poll_attributes=\"30\"\n", wantPoll: 30 * time.Second, want: 35 * time.Second},
		"long poll":        {config: "poll_attributes=\"1800\"\n", wantPoll: 30 * time.Minute, want: 30*time.Minute + 5*time.Second},
		"missing setting":  {config: "spindownDelay=\"0\"\n", want: 2 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk.cfg")
			if err := os.WriteFile(path, []byte(value.config), 0600); err != nil {
				t.Fatal(err)
			}
			poll, got, err := diskPollingIntervals(path)
			if err != nil || poll != value.wantPoll || got != value.want {
				t.Fatalf("poll/grace = %v/%v, %v; want %v/%v", poll, got, err, value.wantPoll, value.want)
			}
		})
	}
}

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
	disks, err := newDiskReader(p, time.Minute).read()
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

func TestReadDisksMarksInvalidTemperatureUnavailable(t *testing.T) {
	for _, temperature := range []string{"broken", "NaN", "+Inf", "-Inf", "-20", "255", "10000"} {
		t.Run(temperature, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "disks.ini")
			data := "[disk1]\nid=serial1\ndevice=sdb\ntemp=" + temperature + "\nrotational=1\n" +
				"[disk2]\nid=serial2\ndevice=sdc\ntemp=35\nrotational=1\n"
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			disks, err := newDiskReader(p, time.Minute).read()
			if err != nil {
				t.Fatal(err)
			}
			if len(disks) != 2 || !disks[0].Unavailable || disks[0].Temp != 0 {
				t.Fatalf("invalid temperature should affect only its disk: %#v", disks)
			}
			if disks[1].Unavailable || disks[1].Temp != 35 {
				t.Fatalf("invalid temperature should not affect another disk: %#v", disks)
			}
		})
	}
}

func TestSelectDisksExcludesExternalDisksFromKindSelectors(t *testing.T) {
	disks := []sensors.Disk{
		{Name: "internal", Device: "sdb", Transport: "ata", Rotational: true},
		{Name: "external", Device: "sdc", Transport: "usb", Rotational: true},
	}

	if got := selectDisks(disks, "hdd"); len(got) != 1 || got[0].Name != "internal" {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "external"); len(got) != 1 || got[0].Name != "external" {
		t.Fatalf("explicit name: %#v", got)
	}
	if got := selectDisks(disks, "all"); len(got) != 2 {
		t.Fatalf("all should include external disks: %#v", got)
	}
}

func TestReadDisksMarksMissingTemperatureOnActiveDiskPending(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(p, []byte("[disk1]\nid=serial\ndevice=sdb\ntemp=*\nrotational=1\nspundown=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	disks, err := readDisks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].state != diskStatePending {
		t.Fatalf("missing temperature should mark the raw disk pending: %#v", disks)
	}
}
