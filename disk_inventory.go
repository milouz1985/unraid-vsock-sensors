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
	eligible        bool
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
			Selected: entry.selected, Eligible: entry.eligible, ValidationError: entry.validationError,
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
	flashIDs := make(map[string]struct{})
	flashDevices := make(map[string]struct{})
	for _, entry := range assigned {
		if entry.disk.id != "" {
			assignedIDs[entry.disk.id] = struct{}{}
		}
		if strings.EqualFold(entry.disk.name, "flash") {
			if entry.disk.id != "" {
				flashIDs[entry.disk.id] = struct{}{}
			}
			if entry.disk.device != "" {
				flashDevices[entry.disk.device] = struct{}{}
			}
		}
	}
	unassigned, err := readUnassignedEntries(devsINIPath, selector, assignedIDs, flashIDs, flashDevices)
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
		if !entry.eligible {
			return nil, selectedDiskValidationError(entry)
		}
		disks = append(disks, entry.disk)
	}
	return disks, nil
}

func selectedDiskValidationError(entry diskInventoryEntry) error {
	disk := entry.disk
	prefix := "disk"
	if entry.source == diskInventoryUnassigned {
		prefix = "unassigned disk"
	}
	if disk.id == "" {
		if entry.source == diskInventoryAssigned {
			return fmt.Errorf("active disk %q has no stable ID", disk.name)
		}
		return fmt.Errorf("unassigned disk %q has no stable ID", disk.name)
	}
	if len(disk.id) > maxUnraidDiskIDSize {
		if entry.source == diskInventoryAssigned {
			return fmt.Errorf("active disk %q has a %d-byte stable ID; observed emhttpd limit is %d", disk.name, len(disk.id), maxUnraidDiskIDSize)
		}
		return fmt.Errorf("unassigned disk %q has a %d-byte stable ID; observed emhttpd limit is %d", disk.name, len(disk.id), maxUnraidDiskIDSize)
	}
	if disk.device == "" {
		return fmt.Errorf("active disk %q has no device", disk.name)
	}
	return fmt.Errorf("%s %q: %s", prefix, disk.name, entry.validationError)
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
		eligible, validationError := diskEligibility(disk, thermalError)
		entries = append(entries, diskInventoryEntry{
			disk: disk, bus: bus, policy: policy, source: diskInventoryAssigned,
			selected: selected, eligible: eligible, validationError: validationError,
		})
	}
	if sections == 0 {
		return nil, errors.New("disk inventory contains no sections")
	}
	return entries, nil
}

func readUnassignedEntries(devsINIPath string, selector *diskSelector, assignedIDs, flashIDs, flashDevices map[string]struct{}) ([]diskInventoryEntry, error) {
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
		if _, flash := flashIDs[id]; flash {
			continue
		}
		if _, flash := flashDevices[device]; flash {
			continue
		}
		bus, policy, selected := selector.evaluate(id, name, device)
		disk, thermalError := diskFromSection(section, unraidDisk{
			id: id, name: name, device: device, transport: transport,
		})
		eligible, validationError := diskEligibility(disk, thermalError)
		entry := diskInventoryEntry{
			disk: disk, bus: bus, policy: policy, source: diskInventoryUnassigned,
			selected: selected, eligible: eligible, validationError: validationError,
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

// diskEligibility reports whether an inventory row currently has the fields
// required by the thermal collector. Invalid rows remain representable so the
// WebUI can expose and exclude them without reparsing the source section.
func diskEligibility(disk unraidDisk, thermalError string) (bool, string) {
	if disk.id == "" {
		return false, "missing stable ID"
	}
	if len(disk.id) > maxUnraidDiskIDSize {
		return false, fmt.Sprintf("stable ID is %d bytes; observed emhttpd limit is %d", len(disk.id), maxUnraidDiskIDSize)
	}
	if disk.device == "" {
		return false, "missing or invalid device"
	}
	if thermalError != "" {
		return false, thermalError
	}
	return true, ""
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
