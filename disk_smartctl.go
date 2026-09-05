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

const smartctlTypePath = "/usr/local/sbin/smartctl_type"

type smartctlReport struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
	} `json:"smartctl"`
	PowerMode struct {
		Name string `json:"name"`
	} `json:"power_mode"`
	Temperature struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
}

func readSMARTTemperature(ctx context.Context, disk unraidDisk) (float64, bool, error) {
	options := "--json -n standby,3 -A"
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
	return cmd.Output()
}

func parseSMARTTemperature(name string, output []byte, runErr error) (float64, bool, error) {
	var report smartctlReport
	if err := json.Unmarshal(output, &report); err != nil {
		return 0, false, fmt.Errorf("decode SMART report for %s: %w", name, errors.Join(runErr, err))
	}
	mode := strings.ToUpper(report.PowerMode.Name)
	if report.Smartctl.ExitStatus == 3 && (mode == "STANDBY" || mode == "SLEEP") {
		return 0, true, nil
	}
	if report.Temperature.Current == nil {
		if runErr != nil {
			return 0, false, fmt.Errorf("read SMART temperature for %s: %w", name, runErr)
		}
		return 0, false, fmt.Errorf("SMART report for %s contains no temperature (exit status %d)", name, report.Smartctl.ExitStatus)
	}
	temperature := *report.Temperature.Current
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 150 {
		return 0, false, fmt.Errorf("SMART report for %s contains invalid temperature %v", name, temperature)
	}
	// smartctl uses non-zero bits for health warnings as well as command errors.
	// A normalized temperature is still a valid reading in the warning case.
	return temperature, false, nil
}
