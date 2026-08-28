package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

func runStorCLI(ctx context.Context, operation string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("storcli")
	if err != nil {
		return nil, fmt.Errorf("%w: storcli is not installed", errHBABackendUnavailable)
	}
	command := exec.CommandContext(ctx, path, args...)
	out, err := command.Output()
	if ctx.Err() != nil {
		return nil, storcliContextError(operation, ctx.Err())
	}
	if err != nil {
		return nil, storcliCommandError(operation, err)
	}
	return out, nil
}

func readStorCLITemperatures(ctx context.Context, _ []int) (map[int]float64, error) {
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
	return parseStorCLIMetadata(out)
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
	Model        string `json:"Model"`
	ProductName  string `json:"Product Name"`
	SerialNumber string `json:"Serial Number"`
	SASAddress   string `json:"SAS Address"`
	PCIAddress   string `json:"PCI Address"`
}

func parseStorCLIMetadata(data []byte) (map[int]hbaMetadata, error) {
	var root struct {
		Controllers []struct {
			CommandStatus struct {
				Controller int    `json:"Controller"`
				Status     string `json:"Status"`
			} `json:"Command Status"`
			ResponseData struct {
				Basics     storCLIBasics `json:"Basics"`
				Model      string        `json:"Model"`
				Product    string        `json:"Product Name"`
				Serial     string        `json:"Serial Number"`
				SASAddress string        `json:"SAS Address"`
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
	metadata, ids := make(map[int]hbaMetadata), make(map[string]int)
	for _, controller := range root.Controllers {
		number := controller.CommandStatus.Controller
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", number, controller.CommandStatus.Status)
		}
		d := controller.ResponseData
		serial := firstHBAValue(d.Basics.SerialNumber, d.Serial)
		sas := firstHBAValue(d.Basics.SASAddress, d.SASAddress)
		pci := normalizePCIAddress(firstHBAValue(d.Basics.PCIAddress, d.PCIAddress))
		model := firstHBAValue(d.Basics.Model, d.Basics.ProductName, d.Model, d.Product)
		id := hbaStableID(serial, sas, pci)
		if id == "" {
			return nil, fmt.Errorf("storcli controller %d has no stable identity", number)
		}
		if previous, duplicate := ids[id]; duplicate {
			return nil, fmt.Errorf("storcli controllers %d and %d have duplicate identity %q", previous, number, id)
		}
		ids[id], metadata[number] = number, hbaMetadata{id: id, model: model, pciAddress: pci}
	}
	return metadata, nil
}

func firstHBAValue(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		switch strings.ToLower(value) {
		case "", "n/a", "na", "none", "unknown":
			continue
		}
		return value
	}
	return ""
}

func hbaStableID(serial, sasAddress, pciAddress string) string {
	if serial = firstHBAValue(serial); serial != "" {
		return "serial:" + strings.ToLower(serial)
	}
	if sasAddress = firstHBAValue(sasAddress); sasAddress != "" {
		return "sas:" + strings.TrimPrefix(strings.ToLower(sasAddress), "0x")
	}
	if pciAddress != "" {
		return "pci:" + pciAddress
	}
	return ""
}

func normalizePCIAddress(address string) string {
	parts := strings.Split(strings.TrimSpace(address), ":")
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

func parseStorCLI(data []byte) (map[int]float64, error) {
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
	temperatures := make(map[int]float64, len(root.Controllers))
	for _, controller := range root.Controllers {
		id := controller.CommandStatus.Controller
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", id, controller.CommandStatus.Status)
		}
		found := false
		for _, property := range controller.ResponseData.ControllerProperties {
			if property.Property != "ROC temperature(Degree Celsius)" {
				continue
			}
			temp, err := strconv.ParseFloat(property.Value, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) || temp < 0 || temp > 150 {
				return nil, fmt.Errorf("storcli controller %d invalid temperature %q", id, property.Value)
			}
			temperatures[id], found = temp, true
			break
		}
		if !found {
			return nil, fmt.Errorf("storcli controller %d has no ROC temperature", id)
		}
	}
	return temperatures, nil
}
