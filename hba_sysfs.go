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

type sysfsHBA struct {
	hostPath string
	driver   string
	metadata hbaMetadata
}

// discoverSysfsHBAs returns the common sysfs identity of every supported HBA.
// Backend-specific controller numbers are resolved separately.
func discoverSysfsHBAs(ctx context.Context, root string) ([]sysfsHBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read SCSI host inventory: %w", err)
	}

	hbas := make([]sysfsHBA, 0, len(entries))
	metadataByPCI := make(map[string]hbaMetadata)
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
		if previous, duplicate := metadataByPCI[pci]; duplicate {
			return nil, fmt.Errorf(
				"SCSI hosts for PCI controller %s appear more than once (previous identity %q)",
				pci,
				previous.id,
			)
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
		metadata := hbaMetadata{id: id, model: model, pciAddress: pci}
		metadataByPCI[pci] = metadata
		hbas = append(hbas, sysfsHBA{hostPath: host, driver: driver, metadata: metadata})
	}
	if len(hbas) == 0 {
		return nil, errNoHBA
	}
	return hbas, nil
}

func sysfsHBAMetadataByPCI(hbas []sysfsHBA) map[string]hbaMetadata {
	metadataByPCI := make(map[string]hbaMetadata, len(hbas))
	for _, hba := range hbas {
		metadataByPCI[hba.metadata.pciAddress] = hba.metadata
	}
	return metadataByPCI
}

func mpt3HBAMetadataByIOC(ctx context.Context, hbas []sysfsHBA) (map[int]hbaMetadata, error) {
	metadataByIOC := make(map[int]hbaMetadata)
	for _, hba := range hbas {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hba.driver != "mpt3sas" {
			continue
		}
		ioc, err := readMPT3IOC(filepath.Join(hba.hostPath, "unique_id"))
		if err != nil {
			return nil, fmt.Errorf("read %s unique ID: %w", filepath.Base(hba.hostPath), err)
		}
		if previous, duplicate := metadataByIOC[ioc]; duplicate {
			return nil, fmt.Errorf(
				"mpt3sas IOC %d appears more than once at %s and %s",
				ioc,
				previous.pciAddress,
				hba.metadata.pciAddress,
			)
		}
		metadataByIOC[ioc] = hba.metadata
	}
	return metadataByIOC, nil
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
