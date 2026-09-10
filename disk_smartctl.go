// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	smartctlTypePath = "/usr/local/sbin/smartctl_type"
	// smartctl returns a bitmask: 0x07 covers command, open/identify and SMART
	// failures. For -n standby, use the recommended status 3 instead of the
	// ambiguous default 2:
	// https://github.com/smartmontools/smartmontools/blob/main/src/smartctl.8.in#L896-L915
	smartctlStandbyExitStatus = 3
	smartctlCommandErrorMask  = 0x07
)

type smartctlReport struct {
	Smartctl struct {
		ExitStatus *int `json:"exit_status"`
	} `json:"smartctl"`
	PowerMode struct {
		Name string `json:"name"`
	} `json:"power_mode"`
	Temperature struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
}

func readSMARTTemperature(ctx context.Context, disk unraidDisk) (float64, bool, error) {
	options := fmt.Sprintf("--json -n standby,%d -A", smartctlStandbyExitStatus)
	if strings.EqualFold(disk.transport, "nvme") {
		options = "--json -A"
	}
	output, runErr := runSMARTCTLType(ctx, disk.name, options)
	return parseSMARTTemperature(disk.name, output, runErr)
}

func runSMARTCTLType(ctx context.Context, name, options string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, smartctlTypePath, name, options)
	// smartctl_type is a PHP wrapper which starts smartctl as a child. Put both
	// processes in their own group so a timeout cannot leave smartctl running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return output, ctxErr
	}
	return output, err
}

func parseSMARTTemperature(name string, output []byte, runErr error) (float64, bool, error) {
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return 0, false, fmt.Errorf("run SMART command for %s: %w", name, runErr)
	}
	var report smartctlReport
	if err := json.Unmarshal(output, &report); err != nil {
		return 0, false, fmt.Errorf("decode SMART report for %s: %w", name, errors.Join(runErr, err))
	}
	if report.Smartctl.ExitStatus == nil {
		return 0, false, fmt.Errorf("SMART report for %s contains no exit status", name)
	}
	exitStatus := *report.Smartctl.ExitStatus
	if exitStatus < 0 || exitStatus > 255 {
		return 0, false, fmt.Errorf("SMART report for %s contains invalid exit status %d", name, exitStatus)
	}
	mode := strings.ToUpper(report.PowerMode.Name)
	// Trust status 3 only with its matching power_mode from the same JSON report;
	// smartctl exposes no stronger discriminator.
	if exitStatus == smartctlStandbyExitStatus && (mode == "STANDBY" || mode == "SLEEP") {
		return 0, true, nil
	}
	if exitStatus&smartctlCommandErrorMask != 0 {
		statusErr := fmt.Errorf("SMART command for %s failed with exit status %d", name, exitStatus)
		return 0, false, errors.Join(statusErr, runErr)
	}
	if exitStatus == 0 && runErr != nil {
		return 0, false, fmt.Errorf("SMART command for %s returned exit status %d but process failed: %w", name, exitStatus, runErr)
	}
	if report.Temperature.Current == nil {
		return 0, false, fmt.Errorf("SMART report for %s contains no temperature (exit status %d)", name, exitStatus)
	}
	temperature := *report.Temperature.Current
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 150 {
		return 0, false, fmt.Errorf("SMART report for %s contains invalid temperature %v", name, temperature)
	}
	// smartctl uses non-zero bits for health warnings as well as command errors.
	// A normalized temperature is still a valid reading in the warning case.
	return temperature, false, nil
}
