package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultHWMonInterval  = time.Second
	virtTempDevicePath    = "/dev/virt-temp"
	defaultHWMonCache     = "/var/lib/unraid-vsock-sensors/hwmon-inventory.json"
	systemdRestartTimeout = 10 * time.Second
	restartRetryDelay     = 30 * time.Second
)

type hwmonPublisher struct {
	disks      hwmonInventory
	hbas       hwmonInventory
	cachePath  string
	cacheDirty bool
}

func hwmon(args []string) error {
	fs := flag.NewFlagSet("hwmon", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	interval := fs.Duration("interval", defaultHWMonInterval, "temperature update interval")
	device := fs.String("device", virtTempDevicePath, "virt-temp control device")
	cache := fs.String("cache", defaultHWMonCache, "persistent hwmon inventory cache")
	restartUnitsFlag := fs.String("restart-units", "", "comma-separated systemd units restarted after a topology change")
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
	if strings.TrimSpace(*cache) == "" {
		return errors.New("cache path must not be empty")
	}
	restartUnits, err := parseRestartUnits(*restartUnitsFlag)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("publishing fixed Unraid hwmon inventories through %s every %s", *device, *interval)
	publisher := &hwmonPublisher{cachePath: *cache}
	err = publisher.restore(*device)
	if err != nil {
		log.Printf("hwmon inventory cache warning: %s", err)
	}
	// Restoring the cache creates the expected virtual sensors before the guest is
	// reachable, but it is too early to restart consumers: CoolerControl could
	// still retain disks discovered through drivetemp before the host released the
	// HBA to the VM. Wait for the first successful VSOCK response, then restart the
	// consumer so it drops those stale disks and discovers the virtual sensors.
	lastError, restartPending := "", false
	receivedGuestResponse := false
	restartAfter := time.Time{}
	for {
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		state, err := sensors.Fetch(requestCtx, uint32(*cid), uint32(*port))
		cancel()
		if err == nil {
			// Only a completed VSOCK request makes the guest-backed inventory
			// authoritative and permits the initial consumer restart.
			firstGuestResponse := !receivedGuestResponse
			receivedGuestResponse = true
			reconfigured, publishErr := publisher.publish(*device, state)
			err = publishErr
			// The first response makes the guest-backed inventory authoritative even
			// when it matches the cache, so consumers must discard stale disk entries.
			if reconfigured || firstGuestResponse {
				restartPending = true
			}
		}
		if restartPending && !time.Now().Before(restartAfter) {
			if restartErr := restartSystemdUnits(ctx, restartUnits); restartErr != nil {
				restartAfter = time.Now().Add(restartRetryDelay)
				log.Printf("topology consumer restart warning: %s", restartErr)
			} else {
				restartPending = false
				restartAfter = time.Time{}
				if len(restartUnits) != 0 {
					log.Printf("restarted topology consumers: %s", strings.Join(restartUnits, ", "))
				}
			}
		}
		if err != nil && err.Error() != lastError {
			lastError = err.Error()
			log.Printf("hwmon update warning; unavailable sensors will apply their failsafe: %s", lastError)
		} else if err == nil && lastError != "" {
			log.Printf("hwmon updates recovered")
			lastError = ""
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

func (publisher *hwmonPublisher) publish(device string, state sensors.Response) (bool, error) {
	disks, hbas := makeHWMonSamples(state)
	var diskErr, hbaErr error
	reconfigured := false
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if changed, err := publishHWMonFamily(device, "disk", &publisher.disks, disks, false); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured storage hwmon inventory with %d channels", len(disks))
		}
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if changed, err := publishHWMonFamily(device, "hba", &publisher.hbas, hbas, state.HBADisabled); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured HBA hwmon inventory with %d channels", len(hbas))
		}
	}
	if reconfigured {
		publisher.cacheDirty = true
	}
	if publisher.cacheDirty {
		if err := publisher.saveCache(); err != nil {
			return reconfigured, errors.Join(diskErr, hbaErr, fmt.Errorf("save hwmon inventory cache: %w", err))
		}
		publisher.cacheDirty = false
	}
	return reconfigured, errors.Join(diskErr, hbaErr)
}

func parseRestartUnits(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var units []string
	seen := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		unit := strings.TrimSpace(item)
		if unit == "" || strings.HasPrefix(unit, "-") || strings.ContainsAny(unit, " \t\r\n/") {
			return nil, fmt.Errorf("invalid systemd unit %q", unit)
		}
		if unit == "unraid-vsock-hwmon.service" {
			return nil, errors.New("unraid-vsock-hwmon.service cannot restart itself")
		}
		if _, duplicate := seen[unit]; duplicate {
			continue
		}
		seen[unit] = struct{}{}
		units = append(units, unit)
	}
	return units, nil
}

func restartSystemdUnits(ctx context.Context, units []string) error {
	if len(units) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, systemdRestartTimeout)
	defer cancel()
	args := append([]string{"try-restart", "--no-block", "--"}, units...)
	output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart topology consumers: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
