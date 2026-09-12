// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultSysBlockRoot   = "/sys/class/block"
	defaultDiskPolicyFile = "/boot/config/plugins/unraid-vsock-sensors/disk-policies.json"
)

type diskPolicy string

const (
	diskPolicyAuto    diskPolicy = "auto"
	diskPolicyInclude diskPolicy = "include"
	diskPolicyExclude diskPolicy = "exclude"
)

type diskBus string

const (
	diskBusUSB     diskBus = "USB"
	diskBusNonUSB  diskBus = "non-USB"
	diskBusUnknown diskBus = "unknown"
)

type diskSelector struct {
	sysBlockRoot string
	policies     map[string]diskPolicy
	busCache     map[string]diskBus
}

func (s *diskSelector) evaluate(id, name, device string) (diskBus, diskPolicy, bool) {
	policy := s.policies[id]
	if policy == "" {
		policy = diskPolicyAuto
	}
	bus := s.detectBus(id, device)
	if strings.EqualFold(name, "flash") {
		return bus, policy, false
	}
	switch policy {
	case diskPolicyInclude:
		return bus, policy, true
	case diskPolicyExclude:
		return bus, policy, false
	default:
		return bus, policy, bus != diskBusUSB
	}
}

func (s *diskSelector) detectBus(id, device string) diskBus {
	key := id + "\x00" + device
	if bus, ok := s.busCache[key]; ok {
		return bus
	}
	root := s.sysBlockRoot
	if root == "" {
		root = defaultSysBlockRoot
	}
	usb, err := isUSBBlockDevice(root, device)
	if err != nil {
		return diskBusUnknown
	}
	bus := diskBusNonUSB
	if usb {
		bus = diskBusUSB
	}
	if s.busCache != nil {
		s.busCache[key] = bus
	}
	return bus
}

// isUSBBlockDevice walks from the block device's physical sysfs node toward
// /sys/devices. A missing or inconsistent path is unknown to the caller.
func isUSBBlockDevice(sysBlockRoot, device string) (bool, error) {
	if device == "" || normalizeDiskDevice(device) != device {
		return false, errors.New("invalid block device name")
	}
	root, err := filepath.Abs(sysBlockRoot)
	if err != nil {
		return false, err
	}
	devicesRoot := filepath.Clean(filepath.Join(root, "..", "..", "devices"))
	physical, err := filepath.EvalSymlinks(filepath.Join(root, device, "device"))
	if err != nil {
		return false, fmt.Errorf("resolve block device %s: %w", device, err)
	}
	relative, err := filepath.Rel(devicesRoot, physical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false, fmt.Errorf("block device %s resolves outside sysfs devices", device)
	}
	seenSubsystem := false
	for node := physical; node != devicesRoot; node = filepath.Dir(node) {
		link := filepath.Join(node, "subsystem")
		target, err := filepath.EvalSymlinks(link)
		if errors.Is(err, os.ErrNotExist) {
			// /sys/devices and intermediate topology nodes need not have a bus.
			if _, linkErr := os.Lstat(link); linkErr == nil {
				return false, fmt.Errorf("broken subsystem link at %s", node)
			} else if !errors.Is(linkErr, os.ErrNotExist) {
				return false, fmt.Errorf("inspect subsystem at %s: %w", node, linkErr)
			}
			continue
		}
		if err != nil {
			return false, fmt.Errorf("resolve subsystem at %s: %w", node, err)
		}
		seenSubsystem = true
		if filepath.Base(target) == "usb" {
			return true, nil
		}
	}
	if !seenSubsystem {
		return false, fmt.Errorf("block device %s has no sysfs subsystem", device)
	}
	return false, nil
}

func readDiskPolicies(path string) (map[string]diskPolicy, error) {
	policies := make(map[string]diskPolicy)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return policies, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &policies); err != nil {
		return nil, fmt.Errorf("parse disk policies: %w", err)
	}
	if policies == nil {
		return nil, errors.New("disk policies must be a JSON object")
	}
	for id, policy := range policies {
		if id == "" || len(id) > maxUnraidDiskIDSize || (policy != diskPolicyInclude && policy != diskPolicyExclude) {
			return nil, fmt.Errorf("invalid disk policy for ID %q", id)
		}
	}
	return policies, nil
}

func writeDiskPolicy(path, id string, policy diskPolicy) error {
	if id == "" || len(id) > maxUnraidDiskIDSize {
		return errors.New("disk ID must contain 1 to 79 bytes")
	}
	if policy != diskPolicyAuto && policy != diskPolicyInclude && policy != diskPolicyExclude {
		return fmt.Errorf("invalid disk policy %q", policy)
	}
	policies, err := readDiskPolicies(path)
	if err != nil {
		return err
	}
	if policy == diskPolicyAuto {
		delete(policies, id)
	} else {
		policies[id] = policy
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".disk-policies-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(policies); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
