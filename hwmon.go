package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultHWMonInterval = time.Second
	hwmonClassPath       = "/sys/class/hwmon"
	virtTempName         = "virt_temp"
)

type hwmonTarget struct {
	temperaturePath string
	find            func(string) (string, error)
}

func newHWMonTarget() *hwmonTarget {
	return &hwmonTarget{
		find: func(name string) (string, error) {
			return findHWMon(hwmonClassPath, name)
		},
	}
}

func (t *hwmonTarget) resolve() (string, error) {
	if t.temperaturePath != "" {
		return t.temperaturePath, nil
	}
	hwmonPath, err := t.find(virtTempName)
	if err != nil {
		return "", err
	}
	t.temperaturePath = filepath.Join(hwmonPath, "temp1_input")
	return t.temperaturePath, nil
}

func (t *hwmonTarget) handleWriteError(err error) {
	// A hwmonN directory may disappear and return under another number after a
	// module reload.
	if errors.Is(err, os.ErrNotExist) {
		t.temperaturePath = ""
	}
}

func hwmon(args []string) error {
	fs := flag.NewFlagSet("hwmon", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	interval := fs.Duration("interval", defaultHWMonInterval, "temperature update interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("hwmon does not accept positional arguments")
	}
	if err := vsockaddr.ValidateCID(uint64(*cid)); err != nil {
		return err
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("interval must be greater than zero")
	}

	target := newHWMonTarget()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("publishing Unraid HDD maximum through %s every %s", virtTempName, *interval)
	failed := false
	for {
		temperaturePath, err := target.resolve()
		if err == nil {
			err = publishHDDMaximum(ctx, uint32(*cid), uint32(*port), temperaturePath, sensors.Fetch)
			target.handleWriteError(err)
		}
		if err != nil && !failed {
			log.Printf("hwmon update failed; virt-temp watchdog will apply its failsafe: %v", err)
			failed = true
		} else if err == nil && failed {
			log.Printf("hwmon updates recovered")
			failed = false
		}

		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func findHWMon(root, name string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(root, "hwmon*"))
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(path, "name"))
		if err == nil && strings.TrimSpace(string(data)) == name {
			return path, nil
		}
	}
	return "", fmt.Errorf("hwmon device %q not found", name)
}

func publishHDDMaximum(
	parent context.Context,
	cid uint32,
	port uint32,
	path string,
	fetch func(context.Context, uint32, uint32) (sensors.Response, error),
) error {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	state, err := fetch(ctx, cid, port)
	if err != nil {
		return err
	}
	if state.Error != "" {
		return errors.New(state.Error)
	}

	var disks []sensors.Disk
	for _, disk := range state.Disks {
		if !disk.IsExternal() && disk.Kind() == sensors.DiskKindHDD {
			disks = append(disks, disk)
		}
	}
	temperature := 0.0
	if len(disks) > 0 {
		temperature = sensors.MaxTemperature(disks, func(disk sensors.Disk) float64 {
			return disk.Temp
		})
	}
	milliCelsius := int64(math.Round(temperature * 1000))
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", milliCelsius)), 0644)
}
