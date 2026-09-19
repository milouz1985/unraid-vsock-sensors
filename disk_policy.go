// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	defaultSysBlockRoot   = "/sys/class/block"
	defaultDiskPolicyFile = "/boot/config/plugins/unraid-vsock-sensors/disk-policies.json"
)

// Policy errors remain matchable with errors.Is so callers do not need to
// distinguish failures by parsing error text.
var (
	// ErrInvalidDiskID is returned when a policy targets an empty or oversized
	// stable disk ID.
	ErrInvalidDiskID = errors.New("invalid disk ID")
	// ErrInvalidDiskPolicy is returned when a policy value is not auto,
	// include or exclude.
	ErrInvalidDiskPolicy = errors.New("invalid disk policy")
	// ErrInvalidPoliciesFile is returned when the persisted disk-policies.json
	// file cannot be parsed or holds an inconsistent policy.
	ErrInvalidPoliciesFile = errors.New("disk policies file is invalid")
)

func invalidPoliciesFile(path, reason string) error {
	return fmt.Errorf("%w at %q: %s", ErrInvalidPoliciesFile, path, reason)
}

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
}

func (s *diskSelector) evaluate(id, name, device string) (diskBus, diskPolicy, bool) {
	policy := s.policies[id]
	if policy == "" {
		policy = diskPolicyAuto
	}
	bus := s.detectBus(device)
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

func (s *diskSelector) detectBus(device string) diskBus {
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
		return nil, fmt.Errorf("read disk policies %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &policies); err != nil {
		return nil, invalidPoliciesFile(path, "parse: "+err.Error())
	}
	if policies == nil {
		return nil, invalidPoliciesFile(path, "must be a JSON object")
	}
	for id, policy := range policies {
		if id == "" || len(id) > maxUnraidDiskIDSize {
			return nil, invalidPoliciesFile(path, fmt.Sprintf("invalid ID %q", id))
		}
		if policy != diskPolicyInclude && policy != diskPolicyExclude {
			return nil, invalidPoliciesFile(path, fmt.Sprintf("invalid policy %q for ID %q", policy, id))
		}
	}
	return policies, nil
}

// diskPolicyStore serializes disk policy mutations within the daemon. The
// daemon is the single writer of the policy file, so no cross-process lock is
// needed; the mutex only orders concurrent HTTP requests.
type diskPolicyStore struct {
	mu   sync.Mutex
	path string
}

func newDiskPolicyStore(path string) *diskPolicyStore {
	return &diskPolicyStore{path: path}
}

func (s *diskPolicyStore) Set(id string, policy diskPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == "" || len(id) > maxUnraidDiskIDSize {
		return fmt.Errorf("%w: must contain 1 to %d bytes", ErrInvalidDiskID, maxUnraidDiskIDSize)
	}
	if policy != diskPolicyAuto && policy != diskPolicyInclude && policy != diskPolicyExclude {
		return fmt.Errorf("%w %q", ErrInvalidDiskPolicy, policy)
	}

	policies, err := readDiskPolicies(s.path)
	if err != nil {
		return err
	}
	if policy == diskPolicyAuto {
		if _, exists := policies[id]; !exists {
			return nil
		}
		delete(policies, id)
	} else {
		if policies[id] == policy {
			return nil
		}
		policies[id] = policy
	}
	return writeDiskPoliciesAtomic(s.path, policies)
}

func (s *diskPolicyStore) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect disk policies %q: %w", s.path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("disk policies path %q is a directory", s.path)
	}
	err = os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove disk policies %q: %w", s.path, err)
	}
	return nil
}

// writeDiskPoliciesAtomic persists the policy file with a temporary file,
// flush, sync and rename so a crash never leaves a truncated file.
func writeDiskPoliciesAtomic(path string, policies map[string]diskPolicy) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create directory for disk policies %q: %w", path, err)
	}
	file, err := os.CreateTemp(directory, ".disk-policies-*")
	if err != nil {
		return fmt.Errorf("create temporary disk policies for %q: %w", path, err)
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return fmt.Errorf("set temporary disk policies mode for %q: %w", path, err)
	}
	if err := json.NewEncoder(file).Encode(policies); err != nil {
		file.Close()
		return fmt.Errorf("encode temporary disk policies for %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary disk policies for %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary disk policies for %q: %w", path, err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace disk policies %q: %w", path, err)
	}
	return nil
}
