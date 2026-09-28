// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultSCSIHostRoot = "/sys/class/scsi_host"

// discoverSysfsHBAs returns the identity of every supported HBA indexed by its
// PCI address. The backend-specific controller number is deliberately absent:
// mpt3ctl and StorCLI only use it to associate temperatures with this inventory.
func discoverSysfsHBAs(ctx context.Context, root string) (map[string]hbaMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read SCSI host inventory: %w", err)
	}

	metadata := make(map[string]hbaMetadata)
	identities := make(map[string]string)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(entry.Name(), "host") {
			continue
		}
		host := filepath.Join(root, entry.Name())
		driver, err := readSysfsHBAAttribute(filepath.Join(host, "proc_name"), false)
		if err != nil {
			return nil, fmt.Errorf("read %s driver: %w", entry.Name(), err)
		}
		if driver != "mpt3sas" && driver != "megaraid_sas" {
			continue
		}

		device, err := filepath.EvalSymlinks(filepath.Join(host, "device"))
		if err != nil {
			return nil, fmt.Errorf("resolve %s device: %w", entry.Name(), err)
		}
		pci := pciAddressFromSysfsPath(device)
		if pci == "" {
			return nil, fmt.Errorf("%s device %q has no PCI address", entry.Name(), device)
		}
		if previous, duplicate := metadata[pci]; duplicate {
			return nil, fmt.Errorf("SCSI hosts for PCI controller %s appear more than once (previous identity %q)", pci, previous.id)
		}

		sas, err := readSysfsHBAAttribute(filepath.Join(host, "host_sas_address"), true)
		if err != nil {
			return nil, fmt.Errorf("read %s SAS address: %w", entry.Name(), err)
		}
		model, err := readSysfsHBAAttribute(filepath.Join(host, "board_name"), true)
		if err != nil {
			return nil, fmt.Errorf("read %s board name: %w", entry.Name(), err)
		}
		id := hbaStableID(sas, pci)
		if previous, duplicate := identities[id]; duplicate {
			return nil, fmt.Errorf("SCSI hosts at %s and %s have duplicate identity %q", previous, pci, id)
		}
		identities[id] = pci
		metadata[pci] = hbaMetadata{id: id, model: model, pciAddress: pci, driver: driver}
	}
	if len(metadata) == 0 {
		return nil, errNoHBA
	}
	return metadata, nil
}

func readSysfsHBAAttribute(path string, optional bool) (string, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return hbaIdentityValue(string(value)), nil
}

func pciAddressFromSysfsPath(path string) string {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if pci := normalizePCIAddress(filepath.Base(current)); pci != "" {
			return pci
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
	}
}
