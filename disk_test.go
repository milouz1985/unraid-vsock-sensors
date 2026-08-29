package main

import (
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

func TestDiskReaderUsesZeroWithoutPreviousTemperature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, []byte("[disk1]\nid=serial1\ndevice=sdb\ntemp=*\nspundown=0\nrotational=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reader := newDiskReader(path, time.Minute)
	disks, err := reader.read()
	if err != nil || len(disks) != 1 || disks[0].Temp != 0 || disks[0].Unavailable {
		t.Fatalf("initial grace reading = %#v, %v", disks, err)
	}
}

func TestDiskSpinupGraceUsesPollAttributesAndCap(t *testing.T) {
	for name, value := range map[string]struct {
		config   string
		wantPoll time.Duration
		want     time.Duration
	}{
		"poll plus margin": {config: "poll_attributes=\"30\"\n", wantPoll: 30 * time.Second, want: 35 * time.Second},
		"two minute cap":   {config: "poll_attributes=\"1800\"\n", wantPoll: 30 * time.Minute, want: 2 * time.Minute},
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
	if got := selectDisks(disks, "hdd", false); len(got) != 2 || got[0].Temp != 35 || got[1].Temp != 0 {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "nvme", false); len(got) != 1 || got[0].Temp != 48 {
		t.Fatalf("nvme: %#v", got)
	}
	if got := selectDisks(disks, "fast", false); len(got) != 1 || got[0].Device != "nvme0n1" {
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
			disks, err := readDisks(p)
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

	if got := selectDisks(disks, "hdd", false); len(got) != 1 || got[0].Name != "internal" {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "external", false); len(got) != 1 || got[0].Name != "external" {
		t.Fatalf("explicit name: %#v", got)
	}
	if got := selectDisks(disks, "all", false); len(got) != 2 {
		t.Fatalf("all should include external disks: %#v", got)
	}
}

func TestSelectDisksCanExcludeUnavailableDisks(t *testing.T) {
	disks := []sensors.Disk{
		{Name: "disk1", Rotational: true, Temp: 35},
		{Name: "disk2", Rotational: true, Unavailable: true},
	}
	if got := selectDisks(disks, "hdd", true); len(got) != 1 || got[0].Name != "disk1" {
		t.Fatalf("available HDDs: %#v", got)
	}
	if got := selectDisks(disks, "hdd", false); len(got) != 2 {
		t.Fatalf("all HDDs: %#v", got)
	}
}

func TestReadDisksMarksMissingTemperatureOnActiveDiskUnavailable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(p, []byte("[disk1]\nid=serial\ndevice=sdb\ntemp=*\nrotational=1\nspundown=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	disks, err := readDisks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || !disks[0].Unavailable {
		t.Fatalf("missing temperature should affect only its disk: %#v", disks)
	}
}
