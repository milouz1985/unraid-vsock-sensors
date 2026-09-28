// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeSysfsValue(value string) *string { return &value }

func addFakeSCSIHost(t *testing.T, root, name, driver, pci, sas, model string, uniqueID *string) {
	t.Helper()
	host := filepath.Join(root, name)
	device := filepath.Join(filepath.Dir(root), "devices", "pci0000:00", pci, name)
	if err := os.MkdirAll(host, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(device, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "proc_name"), []byte(driver+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(host, "device")); err != nil {
		t.Fatal(err)
	}
	if sas != "" {
		if err := os.WriteFile(filepath.Join(host, "host_sas_address"), []byte(sas+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if model != "" {
		if err := os.WriteFile(filepath.Join(host, "board_name"), []byte(model+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if uniqueID != nil {
		if err := os.WriteFile(filepath.Join(host, "unique_id"), []byte(*uniqueID+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverSysfsHBAs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x56C92BF0002E6705", "INSPUR 3008IT", fakeSysfsValue("0"))
	addFakeSCSIHost(t, root, "host3", "megaraid_sas", "0000:07:00.0", "", "", nil)
	addFakeSCSIHost(t, root, "host4", "ahci", "0000:08:00.0", "", "", nil)

	got, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	mpt3 := hbaMetadata{id: "sas:56c92bf0002e6705", model: "INSPUR 3008IT", pciAddress: "0000:06:10.0"}
	megaRAID := hbaMetadata{id: "pci:0000:07:00.0", pciAddress: "0000:07:00.0"}
	want := sysfsHBAInventory{
		metadataByPCI: map[string]hbaMetadata{
			"0000:06:10.0": mpt3,
			"0000:07:00.0": megaRAID,
		},
		mpt3ByIOC: map[int]hbaMetadata{0: mpt3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sysfs HBA inventory = %#v, want %#v", got, want)
	}
}

func TestDiscoverSysfsHBAsAcceptsMPT3IOCRange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5001", "HBA 1", fakeSysfsValue("0"))
	addFakeSCSIHost(t, root, "host3", "mpt3sas", "0000:07:00.0", "0x5002", "HBA 2", fakeSysfsValue("255"))

	inventory, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := inventory.mpt3ByIOC[0].model, "HBA 1"; got != want {
		t.Fatalf("IOC 0 model = %q, want %q", got, want)
	}
	if got, want := inventory.mpt3ByIOC[255].model, "HBA 2"; got != want {
		t.Fatalf("IOC 255 model = %q, want %q", got, want)
	}
}

func TestDiscoverSysfsHBAsRejectsInvalidMPT3IOC(t *testing.T) {
	for _, test := range []struct {
		name     string
		uniqueID *string
		want     string
	}{
		{name: "missing", want: "unique ID"},
		{name: "empty", uniqueID: fakeSysfsValue(""), want: "invalid decimal IOC"},
		{name: "non-numeric", uniqueID: fakeSysfsValue("not-a-number"), want: "invalid decimal IOC"},
		{name: "negative", uniqueID: fakeSysfsValue("-1"), want: "invalid decimal IOC"},
		{name: "above uint8", uniqueID: fakeSysfsValue("256"), want: "invalid decimal IOC"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scsi_host")
			addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5000", "HBA", test.uniqueID)
			if _, err := discoverSysfsHBAs(context.Background(), root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid unique_id returned %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestDiscoverSysfsHBAsRejectsDuplicates(t *testing.T) {
	for _, test := range []struct {
		name      string
		firstPCI  string
		secondPCI string
		firstSAS  string
		secondSAS string
		firstIOC  string
		secondIOC string
		want      string
	}{
		{name: "PCI", firstPCI: "0000:06:10.0", secondPCI: "0000:06:10.0", firstSAS: "0x5001", secondSAS: "0x5002", firstIOC: "1", secondIOC: "2", want: "appear more than once"},
		{name: "identity", firstPCI: "0000:06:10.0", secondPCI: "0000:07:00.0", firstSAS: "0x5001", secondSAS: "0x5001", firstIOC: "1", secondIOC: "2", want: "duplicate identity"},
		{name: "MPT3 IOC", firstPCI: "0000:06:10.0", secondPCI: "0000:07:00.0", firstSAS: "0x5001", secondSAS: "0x5002", firstIOC: "7", secondIOC: "7", want: "IOC 7 appears more than once"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scsi_host")
			addFakeSCSIHost(t, root, "host2", "mpt3sas", test.firstPCI, test.firstSAS, "HBA 1", fakeSysfsValue(test.firstIOC))
			addFakeSCSIHost(t, root, "host3", "mpt3sas", test.secondPCI, test.secondSAS, "HBA 2", fakeSysfsValue(test.secondIOC))
			if _, err := discoverSysfsHBAs(context.Background(), root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("duplicate inventory returned %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestDiscoverSysfsHBAsRejectsIncompleteInventory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5000", "HBA", fakeSysfsValue("0"))
	if err := os.Remove(filepath.Join(root, "host2", "device")); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverSysfsHBAs(context.Background(), root); err == nil || !strings.Contains(err.Error(), "resolve host2 device") {
		t.Fatalf("incomplete sysfs inventory returned %v", err)
	}
}

func TestDiscoverSysfsHBAsHonorsContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverSysfsHBAs(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery returned %v", err)
	}
}
