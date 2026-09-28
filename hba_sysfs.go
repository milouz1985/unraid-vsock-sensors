// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultSCSIHostRoot = "/sys/class/scsi_host"

type sysfsHBAInventory struct {
	metadataByPCI map[string]hbaMetadata
	mpt3ByIOC     map[int]hbaMetadata
}

// discoverSysfsHBAs returns the identity of every supported HBA indexed by its
// PCI address, plus mpt3sas controllers indexed by the IOC number exposed as
// unique_id on the same SCSI host.
func discoverSysfsHBAs(ctx context.Context, root string) (sysfsHBAInventory, error) {
	if err := ctx.Err(); err != nil {
		return sysfsHBAInventory{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return sysfsHBAInventory{}, fmt.Errorf("read SCSI host inventory: %w", err)
	}

	inventory := sysfsHBAInventory{
		metadataByPCI: make(map[string]hbaMetadata),
		mpt3ByIOC:     make(map[int]hbaMetadata),
	}
	identities := make(map[string]string)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return sysfsHBAInventory{}, err
		}
		if !strings.HasPrefix(entry.Name(), "host") {
			continue
		}
		host := filepath.Join(root, entry.Name())
		driver, err := readSysfsHBAAttribute(filepath.Join(host, "proc_name"), false)
		if err != nil {
			return sysfsHBAInventory{}, fmt.Errorf("read %s driver: %w", entry.Name(), err)
		}
		if driver != "mpt3sas" && driver != "megaraid_sas" {
			continue
		}

		device, err := filepath.EvalSymlinks(filepath.Join(host, "device"))
		if err != nil {
			return sysfsHBAInventory{}, fmt.Errorf("resolve %s device: %w", entry.Name(), err)
		}
		pci := pciAddressFromSysfsPath(device)
		if pci == "" {
			return sysfsHBAInventory{}, fmt.Errorf("%s device %q has no PCI address", entry.Name(), device)
		}
		if previous, duplicate := inventory.metadataByPCI[pci]; duplicate {
			return sysfsHBAInventory{}, fmt.Errorf("SCSI hosts for PCI controller %s appear more than once (previous identity %q)", pci, previous.id)
		}

		sas, err := readSysfsHBAAttribute(filepath.Join(host, "host_sas_address"), true)
		if err != nil {
			return sysfsHBAInventory{}, fmt.Errorf("read %s SAS address: %w", entry.Name(), err)
		}
		model, err := readSysfsHBAAttribute(filepath.Join(host, "board_name"), true)
		if err != nil {
			return sysfsHBAInventory{}, fmt.Errorf("read %s board name: %w", entry.Name(), err)
		}
		id := hbaStableID(sas, pci)
		if previous, duplicate := identities[id]; duplicate {
			return sysfsHBAInventory{}, fmt.Errorf("SCSI hosts at %s and %s have duplicate identity %q", previous, pci, id)
		}
		identities[id] = pci
		metadata := hbaMetadata{id: id, model: model, pciAddress: pci}
		inventory.metadataByPCI[pci] = metadata

		if driver == "mpt3sas" {
			ioc, err := readMPT3IOC(filepath.Join(host, "unique_id"))
			if err != nil {
				return sysfsHBAInventory{}, fmt.Errorf("read %s unique ID: %w", entry.Name(), err)
			}
			if previous, duplicate := inventory.mpt3ByIOC[ioc]; duplicate {
				return sysfsHBAInventory{}, fmt.Errorf("mpt3sas IOC %d appears more than once at %s and %s", ioc, previous.pciAddress, pci)
			}
			inventory.mpt3ByIOC[ioc] = metadata
		}
	}
	if len(inventory.metadataByPCI) == 0 {
		return sysfsHBAInventory{}, errNoHBA
	}
	return inventory, nil
}

func readMPT3IOC(path string) (int, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(value))
	ioc, err := strconv.ParseUint(raw, 10, 8)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal IOC %q: %w", raw, err)
	}
	return int(ioc), nil
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
