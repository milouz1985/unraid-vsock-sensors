// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/ini.v1"
)

const maxUnraidDiskIDSize = 79

// unraidDisk is inventory, power state and temperature produced by
// Unraid. smartName locates the matching report in /var/local/emhttp/smart.
type unraidDisk struct {
	id, name, device, transport, temperature, smartName string
	rotational                                          bool
	spundown                                            bool
}

type diskInventoryEntry struct {
	disk     unraidDisk
	bus      diskBus
	policy   diskPolicy
	included bool
}

func readDiskInventory(disksINIPath, devsINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readDiskInventoryEntries(disksINIPath, devsINIPath, selector, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
}

func readDiskInventoryEntries(disksINIPath, devsINIPath string, selector *diskSelector, validateIncluded bool) ([]diskInventoryEntry, error) {
	assigned, err := readAssignedEntries(disksINIPath, selector, validateIncluded)
	if err != nil {
		return nil, err
	}
	flashIDs := make(map[string]struct{})
	flashDevices := make(map[string]struct{})
	for _, entry := range assigned {
		if strings.EqualFold(entry.disk.name, "flash") {
			if entry.disk.id != "" {
				flashIDs[entry.disk.id] = struct{}{}
			}
			if entry.disk.device != "" {
				flashDevices[entry.disk.device] = struct{}{}
			}
		}
	}
	unassigned, err := readUnassignedEntries(devsINIPath, selector, flashIDs, flashDevices, validateIncluded)
	if err != nil {
		return nil, err
	}

	merged := make([]diskInventoryEntry, 0, len(assigned)+len(unassigned))
	seenMergedIDs := make(map[string]struct{}, len(assigned)+len(unassigned))
	// Assigned entries are visited first and win over devs.ini entries with
	// the same stable ID.
	for _, inventory := range [][]diskInventoryEntry{assigned, unassigned} {
		for _, entry := range inventory {
			if _, duplicate := seenMergedIDs[entry.disk.id]; duplicate && entry.disk.id != "" {
				continue
			}
			seenMergedIDs[entry.disk.id] = struct{}{}
			merged = append(merged, entry)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].disk.name == merged[j].disk.name {
			return merged[i].disk.id < merged[j].disk.id
		}
		return merged[i].disk.name < merged[j].disk.name
	})
	return merged, nil
}

func includedDisks(entries []diskInventoryEntry) []unraidDisk {
	var disks []unraidDisk
	for _, entry := range entries {
		if entry.included {
			disks = append(disks, entry.disk)
		}
	}
	return disks
}

func readAssignedEntries(disksINIPath string, selector *diskSelector, validateIncluded bool) ([]diskInventoryEntry, error) {
	config, err := ini.Load(disksINIPath)
	if err != nil {
		return nil, err
	}

	var entries []diskInventoryEntry
	seenIDs := make(map[string]struct{})
	sections := 0
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		sections++
		name := strings.Trim(section.Name(), "\"")
		transport := diskTransport(section)
		id := strings.TrimSpace(section.Key("id").String())
		device := normalizeDiskDevice(section.Key("device").String())
		status := strings.ToUpper(strings.TrimSpace(section.Key("status").String()))
		// Unraid uses the _NP marker for states without a physical disk. Keep
		// every other state so degraded, disabled and emulated disks remain.
		if strings.Contains(status, "_NP") {
			continue
		}
		if id != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				return nil, fmt.Errorf("duplicate disk ID %q in disks.ini", id)
			}
			seenIDs[id] = struct{}{}
		}
		bus, policy, included := selector.evaluate(id, name, device)
		if included && validateIncluded {
			if id == "" {
				return nil, fmt.Errorf("active disk %q has no stable ID", name)
			}
			if len(id) > maxUnraidDiskIDSize {
				return nil, fmt.Errorf("active disk %q has a %d-byte stable ID; observed emhttpd limit is %d", name, len(id), maxUnraidDiskIDSize)
			}
			if device == "" {
				return nil, fmt.Errorf("active disk %q has no device", name)
			}
		}
		disk, err := diskFromSection(section, unraidDisk{
			id: id, name: name, device: device, smartName: name, transport: transport,
		}, included && validateIncluded)
		if err != nil {
			return nil, fmt.Errorf("disk %q: %w", name, err)
		}
		entry := diskInventoryEntry{
			disk: disk,
			bus:  bus, policy: policy, included: included,
		}
		entries = append(entries, entry)
	}
	if sections == 0 {
		return nil, errors.New("disk inventory contains no sections")
	}
	return entries, nil
}

func readUnassignedEntries(devsINIPath string, selector *diskSelector, flashIDs, flashDevices map[string]struct{}, validateIncluded bool) ([]diskInventoryEntry, error) {
	config, err := ini.Load(devsINIPath)
	if err != nil {
		return nil, err
	}

	var entries []diskInventoryEntry
	seenIDs := make(map[string]struct{})
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")
		transport := diskTransport(section)
		id := strings.TrimSpace(section.Key("id").String())
		device := normalizeDiskDevice(section.Key("device").String())
		if id != "" && device != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				return nil, fmt.Errorf("duplicate disk ID %q in devs.ini", id)
			}
			seenIDs[id] = struct{}{}
		}
		if _, flash := flashIDs[id]; flash {
			continue
		}
		if _, flash := flashDevices[device]; flash {
			continue
		}
		bus, policy, included := selector.evaluate(id, name, device)
		if included && validateIncluded {
			if device == "" {
				continue
			}
			if id == "" {
				return nil, fmt.Errorf("unassigned disk %q has no stable ID", name)
			}
			if len(id) > maxUnraidDiskIDSize {
				return nil, fmt.Errorf("unassigned disk %q has a %d-byte stable ID; observed emhttpd limit is %d", name, len(id), maxUnraidDiskIDSize)
			}
		}
		disk, err := diskFromSection(section, unraidDisk{
			id: id, name: name, device: device, smartName: device, transport: transport,
		}, included && validateIncluded)
		if err != nil {
			return nil, fmt.Errorf("unassigned disk %q: %w", name, err)
		}
		entry := diskInventoryEntry{
			disk: disk,
			bus:  bus, policy: policy, included: included,
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func diskTransport(section *ini.Section) string {
	return strings.ToLower(strings.TrimSpace(section.Key("transport").String()))
}

func diskFromSection(section *ini.Section, disk unraidDisk, validateThermalFields bool) (unraidDisk, error) {
	rotational, err := parseBinaryDiskField(section, "rotational", validateThermalFields)
	if err != nil {
		return unraidDisk{}, err
	}
	spundown, err := parseBinaryDiskField(section, "spundown", validateThermalFields)
	if err != nil {
		return unraidDisk{}, err
	}
	disk.temperature = strings.TrimSpace(section.Key("temp").String())
	disk.rotational = rotational
	disk.spundown = spundown
	return disk, nil
}

func parseBinaryDiskField(section *ini.Section, name string, validate bool) (bool, error) {
	value := strings.TrimSpace(section.Key(name).String())
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		if !validate {
			// Invalid rotational/spundown values are tolerated only for excluded
			// or display-only entries, where they are not used thermally.
			return false, nil
		}
		return false, fmt.Errorf("%s must be 0 or 1, got %q", name, value)
	}
}

func normalizeDiskDevice(device string) string {
	device = strings.TrimSpace(device)
	if strings.HasPrefix(device, "/dev/") {
		device = strings.TrimPrefix(device, "/dev/")
	}
	if device == "." || device == ".." || strings.ContainsRune(device, filepath.Separator) {
		return ""
	}
	return device
}
