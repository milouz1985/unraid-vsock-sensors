package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, temperature := range []string{"broken", "NaN", "+Inf", "-Inf"} {
		t.Run(temperature, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "disks.ini")
			data := "[disk1]\nid=serial\ndevice=sdb\ntemp=" + temperature + "\nrotational=1\n"
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			disks, err := readDisks(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(disks) != 1 || !disks[0].Unavailable {
				t.Fatalf("invalid temperature should affect only its disk: %#v", disks)
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
