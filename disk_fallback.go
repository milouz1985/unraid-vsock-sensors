// SPDX-License-Identifier: GPL-3.0-or-later

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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultSDSpinPath       = "/usr/local/sbin/sdspin"
	defaultSmartctlTypePath = "/usr/local/sbin/smartctl_type"
	fallbackCommandTimeout  = 2 * time.Second
	fallbackCycleTimeout    = 4 * time.Second
	fallbackWorkers         = 3
)

func (c *diskCollector) collectFallback(ctx context.Context, disks []unraidDisk) ([]diskObservation, error) {
	observations := make([]diskObservation, len(disks))
	for i, disk := range disks {
		observations[i] = newDirectSMARTObservation(disk)
	}
	cycle, cancel := context.WithTimeout(ctx, fallbackCycleTimeout)
	defer cancel()
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(fallbackWorkers, len(disks)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for cycle.Err() == nil {
				index := int(next.Add(1) - 1)
				if index >= len(disks) {
					return
				}
				observations[index] = c.fallbackObservation(cycle, disks[index])
			}
		}()
	}
	workers.Wait()
	var firstError error
	for _, observation := range observations {
		if observation.err != nil {
			firstError = fmt.Errorf("%s: %w", observation.disk.name, observation.err)
			break
		}
	}
	return observations, firstError
}

func (c *diskCollector) fallbackObservation(ctx context.Context, disk unraidDisk) diskObservation {
	result := newDirectSMARTObservation(disk)
	// sdspin is an ATA check. A rotational disk with another or unknown bus
	// cannot be safely probed here; keep its sample unavailable.
	if disk.rotational {
		if !isATATransport(disk.transport) || disk.device == "" {
			result.err = errors.New("rotational disk has no safe ATA power-state check")
			return result
		}
		path := c.paths.sdspin
		if path == "" {
			path = defaultSDSpinPath
		}
		_, err := runFallbackCommand(ctx, path, "/dev/"+disk.device, "status")
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 2 {
				result.standby = true
				result.err = nil
			}
			if result.err != nil {
				result.err = fmt.Errorf("sdspin status: %w", err)
			}
			return result
		}
	}
	path := c.paths.smartctlType
	if path == "" {
		path = defaultSmartctlTypePath
	}
	// smartctl_type resolves smType, controller ports and the actual device.
	// Its second argument is a single option string, as used by Unraid.
	// The helper takes the section name (disk1 or dev1); unassigned SMART
	// cache filenames instead use the device name (sda, nvme0n1).
	output, err := runFallbackCommand(ctx, path, disk.name, "-n standby,3 -A -j")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		result.standby = true
		result.err = nil
		return result
	}
	if err != nil && len(output) == 0 {
		result.err = fmt.Errorf("smartctl_type: %w", err)
		return result
	}
	direct, parseErr := parseDirectSMART(output)
	if parseErr != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.err = fmt.Errorf("smartctl_type exit %d: %w", exit.ExitCode(), parseErr)
		} else {
			result.err = parseErr
		}
		return result
	}
	result.standby = direct.standby
	result.temperature = direct.temperature
	result.err = nil
	return result
}

func newDirectSMARTObservation(disk unraidDisk) diskObservation {
	return diskObservation{
		disk: disk, source: diskSourceDirect, failure: diskFailureDiscardPrevious,
		err: errors.New("direct SMART temperature unavailable"),
	}
}

func isATATransport(transport string) bool {
	switch strings.ToLower(transport) {
	case "ata", "scsi-sata", "scsi-1ata":
		return true
	default:
		return false
	}
}

func runFallbackCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, fallbackCommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, path, args...)
	// smartctl_type may spawn smartctl. Kill its process group on timeout so a
	// hung child cannot outlive the bounded collection cycle.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = time.Second
	output, err := command.Output()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	return output, err
}

type directSMARTResult struct {
	temperature float64
	standby     bool
}

func parseDirectSMART(output []byte) (directSMARTResult, error) {
	var report struct {
		PowerMode struct {
			Name string `json:"name"`
		} `json:"power_mode"`
		Temperature struct {
			Current *float64 `json:"current"`
		} `json:"temperature"`
		SCSITemperature struct {
			Current *float64 `json:"current"`
		} `json:"scsi_temperature"`
		NVMe struct {
			Temperature *float64 `json:"temperature"`
		} `json:"nvme_smart_health_information_log"`
		ATA struct {
			Table []struct {
				ID  int `json:"id"`
				Raw struct {
					Value  any    `json:"value"`
					String string `json:"string"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return directSMARTResult{}, fmt.Errorf("parse direct SMART JSON: %w", err)
	}
	if strings.EqualFold(report.PowerMode.Name, "standby") {
		return directSMARTResult{standby: true}, nil
	}
	for _, value := range []*float64{report.Temperature.Current, report.SCSITemperature.Current, report.NVMe.Temperature} {
		if value != nil && validDirectTemperature(*value) {
			return directSMARTResult{temperature: *value}, nil
		}
	}
	for _, id := range []int{194, 190} {
		for _, attribute := range report.ATA.Table {
			if attribute.ID != id {
				continue
			}
			if fields := strings.Fields(attribute.Raw.String); len(fields) != 0 {
				if value, err := strconv.ParseFloat(fields[0], 64); err == nil && validDirectTemperature(value) {
					return directSMARTResult{temperature: value}, nil
				}
			}
			var value float64
			var err error
			switch raw := attribute.Raw.Value.(type) {
			case float64:
				value = raw
			case string:
				fields := strings.Fields(raw)
				if len(fields) == 0 {
					continue
				}
				value, err = strconv.ParseFloat(fields[0], 64)
			default:
				continue
			}
			if err == nil && validDirectTemperature(value) {
				return directSMARTResult{temperature: value}, nil
			}
		}
	}
	return directSMARTResult{}, errors.New("direct SMART report has no usable temperature")
}

func validDirectTemperature(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && value < 150
}
