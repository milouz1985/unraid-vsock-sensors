// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"

	"github.com/mdlayher/vsock"
)

const (
	// snapshotStreamTimeout detects a stopped publisher and releases the old
	// connection so Unraid can reconnect. The kernel's stale_timeout remains
	// the thermal failsafe when snapshots do not resume.
	snapshotStreamTimeout = 3 * defaultPublishInterval
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

type receivedSnapshot struct {
	response sensors.Response
	// receivedAt is recorded after the complete frame has been read. It bounds
	// only the time spent waiting in the local publication queue.
	receivedAt time.Time
}

func (snapshot receivedSnapshot) expired(now time.Time) bool {
	return !now.Before(snapshot.receivedAt.Add(snapshotStreamTimeout))
}

func hwmon(args []string) error {
	fs := flag.NewFlagSet("hwmon", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	cache := fs.String("cache", defaultHWMonCache, "persistent hwmon inventory cache")
	restartUnitsFlag := fs.String("restart-units", "", "comma-separated systemd units restarted after hwmon reconfiguration")
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
	if strings.TrimSpace(*cache) == "" {
		return errors.New("cache path must not be empty")
	}
	restartUnits, err := parseRestartUnits(*restartUnitsFlag)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	listener, err := vsock.Listen(uint32(*port), nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", *port, err)
	}
	defer listener.Close()
	log.Printf("receiving Unraid snapshots on VSOCK port %d and publishing them through %s", *port, virtTempDevicePath)
	publisher := &hwmonPublisher{cachePath: *cache}
	err = publisher.restore(virtTempDevicePath)
	if err != nil {
		log.Printf("hwmon inventory cache warning: %s", err)
	}
	// Restoring the cache creates the expected virtual sensors before the guest is
	// reachable, but it is too early to restart consumers: CoolerControl could
	// still retain disks discovered through drivetemp before the host released the
	// HBA to the VM. Wait for a decoded VSOCK snapshot and an initialized hwmon
	// family before restarting consumers to discover the virtual sensors.
	snapshots := make(chan receivedSnapshot, 1)
	backgroundErrors := make(chan error, 1)
	go func() {
		backgroundErrors <- receiveSnapshots(ctx, listener, uint32(*cid), snapshots)
	}()

	updateLog := stickyErrorLog{context: "hwmon update"}
	seenGuestSnapshot := false
	tryRestartConsumers := func() <-chan time.Time {
		if err := restartSystemdUnits(ctx, restartUnits); err != nil {
			log.Printf("topology consumer restart warning: %s; retrying in %s", err, restartRetryDelay)
			return time.After(restartRetryDelay)
		}
		if len(restartUnits) != 0 {
			log.Printf("restarted topology consumers: %s", strings.Join(restartUnits, ", "))
		}
		return nil
	}
	// A nil channel disables the retry case until a failed attempt schedules it.
	var restartRetry <-chan time.Time
	// Main hwmon event loop. The select blocks while no event is ready, so this
	// loop does not poll or run continuously. It wakes only for guest snapshots,
	// receiver failures, deferred consumer restart retries, or context cancellation.
	// A successful consumer restart only disables its retry; only a receiver
	// failure or context cancellation stops the hwmon service.
	for {
		select {
		case snapshot := <-snapshots:
			if snapshot.expired(time.Now()) {
				updateLog.update(fmt.Errorf("discard snapshot queued for %s", time.Since(snapshot.receivedAt).Round(time.Millisecond)))
				continue
			}
			reconfigured, publishErr := publisher.publish(virtTempDevicePath, snapshot.response)
			firstGuestSnapshot := !seenGuestSnapshot
			seenGuestSnapshot = true
			familyInitialized := publisher.disks.initialized || publisher.hbas.initialized
			// The first guest snapshot also restarts consumers when a family
			// already exists from cache, even if no reconfiguration was needed.
			if (reconfigured || (firstGuestSnapshot && familyInitialized)) && restartRetry == nil {
				restartRetry = tryRestartConsumers()
			}
			updateLog.update(publishErr)
		case err := <-backgroundErrors:
			if err == nil && ctx.Err() != nil {
				return nil
			}
			return err
		case <-restartRetry:
			restartRetry = tryRestartConsumers()
		case <-ctx.Done():
			return nil
		}
	}
}

func receiveSnapshots(
	ctx context.Context,
	listener net.Listener,
	expectedCID uint32,
	out chan receivedSnapshot,
) error {
	streamLog := stickyErrorLog{context: "VSOCK snapshot stream"}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept VSOCK publisher: %w", err)
		}
		peer, ok := conn.RemoteAddr().(*vsock.Addr)
		if !ok || peer.ContextID != expectedCID {
			_ = conn.Close()
			continue
		}
		reader := sensors.NewFrameReader(conn)
		for {
			streamErr := conn.SetReadDeadline(time.Now().Add(snapshotStreamTimeout))
			if streamErr == nil {
				var snapshot sensors.Response
				snapshot, streamErr = reader.Read()
				if streamErr == nil {
					streamLog.update(nil)
					received := receivedSnapshot{response: snapshot, receivedAt: time.Now()}
					if !sendLatestSnapshot(ctx, out, received) {
						_ = conn.Close()
						return nil
					}
					continue
				}
			}
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil
			}
			streamLog.update(streamErr)
			break
		}
	}
}

func sendLatestSnapshot(ctx context.Context, out chan receivedSnapshot, snapshot receivedSnapshot) bool {
	select {
	case out <- snapshot:
		return true
	case <-ctx.Done():
		return false
	default:
	}

	// Keep a single pending snapshot. If publication is temporarily busy, a
	// newer heartbeat supersedes the older one instead of creating a backlog.
	select {
	case <-out:
	default:
	}
	select {
	case out <- snapshot:
		return true
	case <-ctx.Done():
		return false
	}
}

func (publisher *hwmonPublisher) publish(device string, state sensors.Response) (bool, error) {
	disks, hbas := makeHWMonSamples(state)
	var diskErr, hbaErr error
	reconfigured := false
	// A non-nil empty inventory is authoritative and removes the last cached
	// family instead of leaving a permanent failsafe device behind.
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if state.Disks == nil {
		diskErr = errors.New("disks: inventory is missing; waiting for sensors")
	} else if changed, err := publishHWMonFamily(device, "disk", &publisher.disks, disks); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured storage hwmon inventory with %d sensors", len(disks))
		}
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if state.HBAs == nil {
		hbaErr = errors.New("HBA: inventory is missing; waiting for sensors")
	} else if changed, err := publishHWMonFamily(device, "hba", &publisher.hbas, hbas); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured HBA hwmon inventory with %d sensors", len(hbas))
		}
	}
	if reconfigured {
		publisher.cacheDirty = true
	}
	// Clear cacheDirty only after a durable save. On failure, the next snapshot
	// retries persistence even if no further topology change occurs.
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
		if unit == "" || strings.HasPrefix(unit, "-") || strings.ContainsAny(unit, " \t\r\n/*?[]") {
			return nil, fmt.Errorf("invalid systemd unit %q", unit)
		}
		if unit == "unraid-vsock-hwmon" || unit == "unraid-vsock-hwmon.service" {
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
