// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
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
	addFakeSCSIHost(
		t,
		root,
		"host2",
		"mpt3sas",
		"0000:06:10.0",
		"0x56C92BF0002E6705",
		"INSPUR 3008IT",
		fakeSysfsValue("0"),
	)
	addFakeSCSIHost(t, root, "host3", "megaraid_sas", "0000:07:00.0", "", "", nil)
	addFakeSCSIHost(t, root, "host4", "ahci", "0000:08:00.0", "", "", nil)

	hbas, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	mpt3 := hbaMetadata{id: "sas:56c92bf0002e6705", model: "INSPUR 3008IT", pciAddress: "0000:06:10.0"}
	megaRAID := hbaMetadata{id: "pci:0000:07:00.0", pciAddress: "0000:07:00.0"}
	want := map[string]hbaMetadata{
		"0000:06:10.0": mpt3,
		"0000:07:00.0": megaRAID,
	}
	if got := sysfsHBAMetadataByPCI(hbas); !reflect.DeepEqual(got, want) {
		t.Fatalf("sysfs HBA metadata = %#v, want %#v", got, want)
	}
}

func TestMPT3HBAMetadataByIOCAcceptsRange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5001", "HBA 1", fakeSysfsValue("0"))
	addFakeSCSIHost(t, root, "host3", "mpt3sas", "0000:07:00.0", "0x5002", "HBA 2", fakeSysfsValue("255"))

	hbas, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	metadataByIOC, err := mpt3HBAMetadataByIOC(context.Background(), hbas)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := metadataByIOC[0].model, "HBA 1"; got != want {
		t.Fatalf("IOC 0 model = %q, want %q", got, want)
	}
	if got, want := metadataByIOC[255].model, "HBA 2"; got != want {
		t.Fatalf("IOC 255 model = %q, want %q", got, want)
	}
}

func TestMPT3HBAMetadataByIOCRejectsInvalidValues(t *testing.T) {
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
			hbas, err := discoverSysfsHBAs(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = mpt3HBAMetadataByIOC(context.Background(), hbas)
			if err == nil || !strings.Contains(err.Error(), test.want) {
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
		want      string
	}{
		{
			name:      "PCI",
			firstPCI:  "0000:06:10.0",
			secondPCI: "0000:06:10.0",
			firstSAS:  "0x5001",
			secondSAS: "0x5002",
			want:      "appear more than once",
		},
		{
			name:      "identity",
			firstPCI:  "0000:06:10.0",
			secondPCI: "0000:07:00.0",
			firstSAS:  "0x5001",
			secondSAS: "0x5001",
			want:      "duplicate identity",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scsi_host")
			addFakeSCSIHost(t, root, "host2", "mpt3sas", test.firstPCI, test.firstSAS, "HBA 1", fakeSysfsValue("1"))
			addFakeSCSIHost(t, root, "host3", "mpt3sas", test.secondPCI, test.secondSAS, "HBA 2", fakeSysfsValue("2"))
			if _, err := discoverSysfsHBAs(context.Background(), root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("duplicate inventory returned %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestMPT3HBAMetadataByIOCRejectsDuplicateIOC(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5001", "HBA 1", fakeSysfsValue("7"))
	addFakeSCSIHost(t, root, "host3", "mpt3sas", "0000:07:00.0", "0x5002", "HBA 2", fakeSysfsValue("7"))
	hbas, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mpt3HBAMetadataByIOC(context.Background(), hbas)
	if err == nil || !strings.Contains(err.Error(), "IOC 7 appears more than once") {
		t.Fatalf("duplicate IOC returned %v", err)
	}
}

func TestInvalidMPT3IOCDoesNotPreventStorCLIMatching(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5001", "HBA", fakeSysfsValue("invalid"))
	hbas, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := hbas[0].metadata
	got, err := matchStorCLIControllers(
		map[int]string{0: "0000:06:10.0"},
		sysfsHBAMetadataByPCI(hbas),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[int]hbaMetadata{0: want}) {
		t.Fatalf("matched controllers = %#v, want metadata %#v", got, want)
	}
}

func TestDiscoverSysfsHBAsRejectsIncompleteInventory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5000", "HBA", fakeSysfsValue("0"))
	if err := os.Remove(filepath.Join(root, "host2", "device")); err != nil {
		t.Fatal(err)
	}
	_, err := discoverSysfsHBAs(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "resolve host2 device") {
		t.Fatalf("incomplete sysfs inventory returned %v", err)
	}
}
