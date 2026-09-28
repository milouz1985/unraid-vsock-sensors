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

func addFakeSCSIHost(t *testing.T, root, name, driver, pci, sas, model string) {
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
}

func TestDiscoverSysfsHBAs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x56C92BF0002E6705", "INSPUR 3008IT")
	addFakeSCSIHost(t, root, "host3", "megaraid_sas", "0000:07:00.0", "", "")
	addFakeSCSIHost(t, root, "host4", "ahci", "0000:08:00.0", "", "")

	got, err := discoverSysfsHBAs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]hbaMetadata{
		"0000:06:10.0": {id: "sas:56c92bf0002e6705", model: "INSPUR 3008IT", pciAddress: "0000:06:10.0", driver: "mpt3sas"},
		"0000:07:00.0": {id: "pci:0000:07:00.0", pciAddress: "0000:07:00.0", driver: "megaraid_sas"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sysfs HBA inventory = %#v, want %#v", got, want)
	}
}

func TestDiscoverSysfsHBAsRejectsIncompleteInventory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5000", "HBA")
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
