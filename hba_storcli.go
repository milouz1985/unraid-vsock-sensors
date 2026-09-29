// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type storCLIReader struct {
	metadata         map[int]hbaMetadata
	discoverMetadata func(context.Context) (map[int]hbaMetadata, error)
	readTemperatures func(context.Context) (map[int]hbaTemperatures, error)
}

func (r *storCLIReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	freshDiscovery := false
	if r.metadata == nil {
		if err := r.discover(ctx); err != nil {
			return nil, err
		}
		freshDiscovery = true
	}
	readings, err := r.read(ctx)
	if err == nil {
		return readings, nil
	}
	r.metadata = nil
	// Rediscover at most once per collection. If discovery already happened in
	// this call, leave the metadata invalidated so the next collection retries.
	if freshDiscovery {
		return nil, err
	}
	if discoveryErr := r.discover(ctx); discoveryErr != nil {
		return nil, errors.Join(err, discoveryErr)
	}
	return r.read(ctx)
}

func (r *storCLIReader) discover(ctx context.Context) error {
	metadata, err := r.discoverMetadata(ctx)
	if err != nil {
		return fmt.Errorf("storcli discovery: %w", err)
	}
	r.metadata = metadata
	return nil
}

func (r *storCLIReader) read(ctx context.Context) ([]sensors.HBA, error) {
	controllers := sortedIntKeys(r.metadata)
	temperatures, err := r.readTemperatures(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateHBAControllerSet(controllers, temperatures); err != nil {
		return nil, err
	}
	return buildHBAReadings(temperatures, r.metadata), nil
}

func validateHBAControllerSet(controllers []int, temperatures map[int]hbaTemperatures) error {
	actual := sortedIntKeys(temperatures)
	if !slices.Equal(controllers, actual) {
		return fmt.Errorf("HBA controller set changed: expected %v, got %v", controllers, actual)
	}
	return nil
}

func sortedIntKeys[V any](values map[int]V) []int {
	return slices.Sorted(maps.Keys(values))
}

func buildHBAReadings(temperatures map[int]hbaTemperatures, metadata map[int]hbaMetadata) []sensors.HBA {
	readings := make([]sensors.HBA, 0, len(temperatures))
	for controller, controllerTemperatures := range temperatures {
		reading, available := makeHBAReading(metadata[controller], controllerTemperatures)
		if available {
			readings = append(readings, reading)
		}
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings
}

func runStorCLI(ctx context.Context, operation string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("storcli")
	if err != nil {
		return nil, fmt.Errorf("%w: storcli is not installed", errHBABackendUnavailable)
	}
	command := exec.CommandContext(ctx, path, args...)
	// StorCLI is executed directly. WaitDelay bounds pipe draining if a
	// descendant keeps stdout or stderr open after the main process exits.
	command.WaitDelay = time.Second
	out, err := command.Output()
	if ctx.Err() != nil {
		return nil, storcliContextError(operation, ctx.Err())
	}
	if err != nil {
		return nil, storcliCommandError(operation, err)
	}
	return out, nil
}

func readStorCLITemperatures(ctx context.Context) (map[int]hbaTemperatures, error) {
	out, err := runStorCLI(ctx, "temperature", "/cALL", "show", "temperature", "J", "nolog")
	if err != nil {
		return nil, err
	}
	return parseStorCLI(out)
}

func discoverStorCLIHBAs(ctx context.Context) (map[int]hbaMetadata, error) {
	out, err := runStorCLI(ctx, "discovery", "/cALL", "show", "J", "nolog")
	if err != nil {
		return nil, err
	}
	controllers, err := parseStorCLIControllers(out)
	if err != nil {
		return nil, err
	}
	inventory, err := discoverSysfsHBAs(ctx, defaultSCSIHostRoot)
	if err != nil {
		return nil, fmt.Errorf("read sysfs HBA identities: %w", err)
	}
	return matchStorCLIControllers(controllers, inventory.metadataByPCI)
}

func matchStorCLIControllers(controllers map[int]string, identities map[string]hbaMetadata) (map[int]hbaMetadata, error) {
	metadata := make(map[int]hbaMetadata, len(controllers))
	ids := make(map[string]int, len(controllers))
	for controller, pci := range controllers {
		identity, found := identities[pci]
		if !found {
			return nil, fmt.Errorf("storcli controller %d at %s is missing from sysfs HBA inventory", controller, pci)
		}
		if previous, duplicate := ids[identity.id]; duplicate {
			return nil, fmt.Errorf("storcli controllers %d and %d have duplicate identity %q", previous, controller, identity.id)
		}
		ids[identity.id], metadata[controller] = controller, identity
	}
	return metadata, nil
}

func storcliContextError(operation string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("storcli %s timeout: %w", operation, err)
	}
	return fmt.Errorf("storcli %s canceled: %w", operation, err)
}

func storcliCommandError(operation string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			return fmt.Errorf("storcli %s: %w: %s", operation, err, stderr)
		}
	}
	return fmt.Errorf("storcli %s: %w", operation, err)
}

type storCLIBasics struct {
	PCIAddress string `json:"PCI Address"`
}

func parseStorCLIControllers(data []byte) (map[int]string, error) {
	var root struct {
		Controllers []struct {
			CommandStatus struct {
				Controller int    `json:"Controller"`
				Status     string `json:"Status"`
			} `json:"Command Status"`
			ResponseData struct {
				Basics     storCLIBasics `json:"Basics"`
				PCIAddress string        `json:"PCI Address"`
			} `json:"Response Data"`
		} `json:"Controllers"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse storcli discovery JSON: %w", err)
	}
	if len(root.Controllers) == 0 {
		return nil, errNoHBA
	}
	controllers := make(map[int]string, len(root.Controllers))
	for _, controller := range root.Controllers {
		number := controller.CommandStatus.Controller
		if _, duplicate := controllers[number]; duplicate {
			return nil, fmt.Errorf("storcli controller %d appears more than once", number)
		}
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", number, controller.CommandStatus.Status)
		}
		d := controller.ResponseData
		pci := normalizePCIAddress(firstHBAValue(d.Basics.PCIAddress, d.PCIAddress))
		if pci == "" {
			return nil, fmt.Errorf("storcli controller %d has no valid PCI address", number)
		}
		controllers[number] = pci
	}
	return controllers, nil
}

func firstHBAValue(values ...string) string {
	for _, value := range values {
		if value = hbaIdentityValue(value); value != "" {
			return value
		}
	}
	return ""
}

func normalizePCIAddress(address string) string {
	address = strings.TrimSpace(address)
	if dot := strings.LastIndexByte(address, '.'); dot > strings.LastIndexByte(address, ':') {
		address = address[:dot] + ":" + address[dot+1:]
	}
	parts := strings.Split(address, ":")
	if len(parts) != 4 {
		return ""
	}
	values := make([]uint64, 4)
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return ""
		}
		values[i] = value
	}
	if values[0] > 0xffff || values[1] > 0xff || values[2] > 0x1f || values[3] > 7 {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", values[0], values[1], values[2], values[3])
}

func parseStorCLI(data []byte) (map[int]hbaTemperatures, error) {
	var root struct {
		Controllers []struct {
			CommandStatus struct {
				Controller int    `json:"Controller"`
				Status     string `json:"Status"`
			} `json:"Command Status"`
			ResponseData struct {
				ControllerProperties []struct {
					Property string `json:"Ctrl_Prop"`
					Value    string `json:"Value"`
				} `json:"Controller Properties"`
			} `json:"Response Data"`
		} `json:"Controllers"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse storcli JSON: %w", err)
	}
	if len(root.Controllers) == 0 {
		return nil, errNoHBA
	}
	temperatures := make(map[int]hbaTemperatures, len(root.Controllers))
	for _, controller := range root.Controllers {
		id := controller.CommandStatus.Controller
		if _, duplicate := temperatures[id]; duplicate {
			return nil, fmt.Errorf("storcli controller %d appears more than once", id)
		}
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", id, controller.CommandStatus.Status)
		}
		controllerTemperatures := hbaTemperatures{}
		for _, property := range controller.ResponseData.ControllerProperties {
			var destination **float64
			switch property.Property {
			case "ROC temperature(Degree Celsius)", "ROC temperature(Degree Celcius)":
				destination = &controllerTemperatures.ioc
			case "Ctrl temperature(Degree Celsius)", "Ctrl temperature(Degree Celcius)",
				"Controller temperature(Degree Celsius)", "Controller temperature(Degree Celcius)":
				destination = &controllerTemperatures.board
			default:
				continue
			}
			if *destination != nil {
				return nil, fmt.Errorf("storcli controller %d has duplicate temperature property %q", id, property.Property)
			}
			temp, err := strconv.ParseFloat(property.Value, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) {
				return nil, fmt.Errorf("storcli controller %d invalid temperature %q", id, property.Value)
			}
			temperature := temp
			*destination = &temperature
		}
		temperatures[id] = controllerTemperatures
	}
	return temperatures, nil
}
