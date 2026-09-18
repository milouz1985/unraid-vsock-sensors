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

// unraidDisk is inventory, power state and temperature produced by Unraid.
type unraidDisk struct {
	id, name, device, transport, temperature string
	rotational                               bool
	spundown                                 bool
}

type diskInventorySource uint8

const (
	diskInventoryAssigned diskInventorySource = iota
	diskInventoryUnassigned
)

type diskInventoryEntry struct {
	disk            unraidDisk
	bus             diskBus
	policy          diskPolicy
	source          diskInventorySource
	selected        bool
	validationError string
}

func readDiskInventory(disksINIPath, devsINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readDiskInventoryEntries(disksINIPath, devsINIPath, selector)
	if err != nil {
		return nil, err
	}
	return selectedDisks(entries)
}

// diskPolicyRow is the stable disk policy inventory exposed by the WebUI
// control API. Selected is the policy/bus decision; Eligible is the independent
// structural validity required by the thermal collector.
type diskPolicyRow struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Device          string     `json:"device"`
	Transport       string     `json:"transport"`
	Bus             diskBus    `json:"bus"`
	Policy          diskPolicy `json:"policy"`
	Selected        bool       `json:"selected"`
	Eligible        bool       `json:"eligible"`
	ValidationError string     `json:"validation_error,omitempty"`
}

func inventoryRowsFromEntries(entries []diskInventoryEntry) []diskPolicyRow {
	rows := make([]diskPolicyRow, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, diskPolicyRow{
			ID: entry.disk.id, Name: entry.disk.name, Device: entry.disk.device,
			Transport: entry.disk.transport, Bus: entry.bus, Policy: entry.policy,
			Selected: entry.selected, Eligible: entry.validationError == "", ValidationError: entry.validationError,
		})
	}
	return rows
}

func readDiskInventoryEntries(disksINIPath, devsINIPath string, selector *diskSelector) ([]diskInventoryEntry, error) {
	assigned, err := readAssignedEntries(disksINIPath, selector)
	if err != nil {
		return nil, err
	}
	assignedIDs := make(map[string]struct{}, len(assigned))
	flashDevices := make(map[string]struct{})
	for _, entry := range assigned {
		if entry.disk.id != "" {
			assignedIDs[entry.disk.id] = struct{}{}
		}
		if strings.EqualFold(entry.disk.name, "flash") {
			if entry.disk.device != "" {
				flashDevices[entry.disk.device] = struct{}{}
			}
		}
	}
	unassigned, err := readUnassignedEntries(devsINIPath, selector, assignedIDs, flashDevices)
	if err != nil {
		return nil, err
	}

	merged := append(assigned, unassigned...)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].disk.name == merged[j].disk.name {
			return merged[i].disk.id < merged[j].disk.id
		}
		return merged[i].disk.name < merged[j].disk.name
	})
	return merged, nil
}

func selectedDisks(entries []diskInventoryEntry) ([]unraidDisk, error) {
	var disks []unraidDisk
	for _, entry := range entries {
		if !entry.selected {
			continue
		}
		// Unassigned Devices may transiently retain rows without a physical
		// device. The strict collector historically ignores those rows while the
		// management inventory keeps them visible for policy changes.
		if entry.source == diskInventoryUnassigned && entry.disk.device == "" {
			continue
		}
		if entry.validationError != "" {
			prefix := "active disk"
			if entry.source == diskInventoryUnassigned {
				prefix = "unassigned disk"
			}
			return nil, fmt.Errorf("%s %q: %s", prefix, entry.disk.name, entry.validationError)
		}
		disks = append(disks, entry.disk)
	}
	return disks, nil
}

func readAssignedEntries(disksINIPath string, selector *diskSelector) ([]diskInventoryEntry, error) {
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
		bus, policy, selected := selector.evaluate(id, name, device)
		disk, thermalError := diskFromSection(section, unraidDisk{
			id: id, name: name, device: device, transport: transport,
		})
		entries = append(entries, diskInventoryEntry{
			disk: disk, bus: bus, policy: policy, source: diskInventoryAssigned,
			selected: selected, validationError: diskValidationError(disk, thermalError),
		})
	}
	if sections == 0 {
		return nil, errors.New("disk inventory contains no sections")
	}
	return entries, nil
}

func readUnassignedEntries(devsINIPath string, selector *diskSelector, assignedIDs, flashDevices map[string]struct{}) ([]diskInventoryEntry, error) {
	config, err := ini.Load(devsINIPath)
	if err != nil {
		return nil, err
	}

	var entries []diskInventoryEntry
	seenIDIndexes := make(map[string]int)
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")
		transport := diskTransport(section)
		id := strings.TrimSpace(section.Key("id").String())
		device := normalizeDiskDevice(section.Key("device").String())
		// Skip copies of assigned disks before any validation. A shadowed entry
		// must not trigger duplicate detection, policy evaluation, or thermal
		// field validation.
		if id != "" {
			if _, assigned := assignedIDs[id]; assigned {
				continue
			}
		}
		if _, flash := flashDevices[device]; flash {
			continue
		}
		bus, policy, selected := selector.evaluate(id, name, device)
		disk, thermalError := diskFromSection(section, unraidDisk{
			id: id, name: name, device: device, transport: transport,
		})
		entry := diskInventoryEntry{
			disk: disk, bus: bus, policy: policy, source: diskInventoryUnassigned,
			selected: selected, validationError: diskValidationError(disk, thermalError),
		}
		if id == "" {
			entries = append(entries, entry)
			continue
		}
		if index, duplicate := seenIDIndexes[id]; duplicate {
			previous := entries[index]
			switch {
			case previous.disk.device != "" && entry.disk.device != "":
				return nil, fmt.Errorf("duplicate disk ID %q in devs.ini", id)
			case previous.disk.device == "" && entry.disk.device != "":
				// devs.ini may transiently retain an incomplete copy of a disk.
				// Prefer the entry that has a usable device instead of letting file
				// order decide which representation of the stable ID survives.
				entries[index] = entry
			}
			continue
		}
		seenIDIndexes[id] = len(entries)
		entries = append(entries, entry)
	}
	return entries, nil
}

// diskValidationError reports why an inventory row cannot be used by the
// thermal collector. Invalid rows remain representable so the WebUI can expose
// and exclude them without reparsing the source section.
func diskValidationError(disk unraidDisk, thermalError string) string {
	if disk.id == "" {
		return "missing stable ID"
	}
	if len(disk.id) > maxUnraidDiskIDSize {
		return fmt.Sprintf("stable ID is %d bytes; observed emhttpd limit is %d", len(disk.id), maxUnraidDiskIDSize)
	}
	if disk.device == "" {
		return "missing or invalid device"
	}
	return thermalError
}

func diskTransport(section *ini.Section) string {
	return strings.ToLower(strings.TrimSpace(section.Key("transport").String()))
}

// diskFromSection parses each thermal field once. Invalid fields are retained
// as eligibility metadata rather than changing parser strictness by caller.
func diskFromSection(section *ini.Section, disk unraidDisk) (unraidDisk, string) {
	rotational, rotationalErr := parseBinaryDiskField(section, "rotational")
	spundown, spundownErr := parseBinaryDiskField(section, "spundown")
	disk.temperature = strings.TrimSpace(section.Key("temp").String())
	disk.rotational = rotational
	disk.spundown = spundown
	if rotationalErr != nil {
		return disk, rotationalErr.Error()
	}
	if spundownErr != nil {
		return disk, spundownErr.Error()
	}
	return disk, ""
}

func parseBinaryDiskField(section *ini.Section, name string) (bool, error) {
	value := strings.TrimSpace(section.Key(name).String())
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
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
