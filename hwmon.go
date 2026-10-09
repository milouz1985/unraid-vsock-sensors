// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"

	"github.com/mdlayher/vsock"
)

const (
	virtTempConfigPath  = "/sys/kernel/config/virt_temp"
	virtTempDeviceDir   = "/dev"
	defaultHWMonCache   = "/var/lib/unraid-vsock-sensors/hwmon-inventory.json"
	topologyChangedPath = "/run/unraid-vsock-sensors/topology-changed"
)

type hwmonPublisher struct {
	disks      hwmonInventory
	hbas       hwmonInventory
	cachePath  string
	cacheDirty bool
}

type hwmonConfig struct {
	cid       uint32
	port      uint32
	cachePath string
}

func hwmon(args []string) error {
	fs := flag.NewFlagSet("hwmon", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	cache := fs.String("cache", defaultHWMonCache, "persistent hwmon inventory cache")
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

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()
	return runHWMon(ctx, hwmonConfig{
		cid:       uint32(*cid),
		port:      uint32(*port),
		cachePath: *cache,
	})
}

func runHWMon(ctx context.Context, config hwmonConfig) error {
	listener, err := vsock.Listen(config.port, nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", config.port, err)
	}
	defer listener.Close()
	log.Printf("receiving Unraid snapshots on VSOCK port %d and publishing them through virt_temp configfs and per-sensor devices", config.port)
	publisher := &hwmonPublisher{cachePath: config.cachePath}
	err = publisher.restore(virtTempConfigPath, virtTempDeviceDir)
	if err != nil {
		log.Printf("hwmon inventory cache warning: %s", err)
	}
	// Restoring the cache creates the expected virtual sensors before the guest is
	// reachable, but it is too early to announce a usable topology. Wait for a
	// decoded VSOCK snapshot and an initialized hwmon family before notifying
	// subscribers that they can rediscover the virtual sensors.
	snapshots := make(chan receivedSnapshot, 1)
	backgroundErrors := make(chan error, 1)
	go func() {
		backgroundErrors <- receiveSnapshots(ctx, listener, config.cid, snapshots)
	}()
	return hwmonOrchestrationLoop(ctx, snapshots, backgroundErrors, publisher, virtTempConfigPath, virtTempDeviceDir, topologyChangedPath)
}

// hwmonOrchestrationLoop is the main hwmon event loop. The select blocks while
// no event is ready, so this loop does not poll or run continuously. It wakes
// only for guest snapshots, receiver failures, or context cancellation.
func hwmonOrchestrationLoop(
	ctx context.Context,
	snapshots <-chan receivedSnapshot,
	backgroundErrors <-chan error,
	publisher *hwmonPublisher,
	configRoot, deviceRoot, topologyPath string,
) error {
	updateLog := stickyErrorLog{context: "hwmon update"}
	seenGuestSnapshot := false
	for {
		select {
		case snapshot := <-snapshots:
			if snapshot.expired(time.Now()) {
				updateLog.update(fmt.Errorf("discard snapshot queued for %s", time.Since(snapshot.receivedAt).Round(time.Millisecond)))
				continue
			}
			reconfigured, publishErr := publisher.publish(configRoot, deviceRoot, snapshot.response)
			firstGuestSnapshot := !seenGuestSnapshot
			seenGuestSnapshot = true
			// The first guest snapshot also announces a topology restored from
			// cache, even if no reconfiguration was needed, but never while
			// another family still needs reconciliation.
			if publisher.shouldNotifyTopologyChanged(reconfigured, firstGuestSnapshot) {
				if err := notifyTopologyChanged(topologyPath); err != nil {
					publishErr = errors.Join(publishErr, err)
				}
			}
			updateLog.update(publishErr)
		case err := <-backgroundErrors:
			if err == nil && ctx.Err() != nil {
				return nil
			}
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

func (publisher *hwmonPublisher) reconciliationPending() bool {
	return publisher.disks.needsReconcile || publisher.hbas.needsReconcile
}

func (publisher *hwmonPublisher) shouldNotifyTopologyChanged(reconfigured, firstGuestSnapshot bool) bool {
	familyInitialized := publisher.disks.sensors != nil || publisher.hbas.sensors != nil
	return !publisher.reconciliationPending() &&
		(reconfigured || firstGuestSnapshot && familyInitialized)
}

func notifyTopologyChanged(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("notify hwmon topology change: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("notify hwmon topology change: %w", err)
	}
	return nil
}

func (publisher *hwmonPublisher) publish(configRoot, deviceRoot string, state sensors.Response) (bool, error) {
	disks, hbas := makeHWMonSamples(state)
	var diskErr, hbaErr error
	var disksReconfigured, hbasReconfigured bool
	// A non-nil empty inventory is authoritative and removes the last cached
	// family instead of leaving a permanent failsafe device behind.
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if state.Disks == nil {
		diskErr = errors.New("disks: inventory is missing; waiting for sensors")
	} else if changed, err := publishHWMonFamily(configRoot, deviceRoot, "disk", &publisher.disks, disks); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	} else {
		disksReconfigured = changed
		if changed {
			log.Printf("configured storage hwmon inventory with %d sensors", len(disks))
		}
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if state.HBAs == nil {
		hbaErr = errors.New("HBA: inventory is missing; waiting for sensors")
	} else if changed, err := publishHWMonFamily(configRoot, deviceRoot, "hba", &publisher.hbas, hbas); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	} else {
		hbasReconfigured = changed
		if changed {
			log.Printf("configured HBA hwmon inventory with %d sensors", len(hbas))
		}
	}
	topologyChanged := disksReconfigured || hbasReconfigured
	if topologyChanged {
		publisher.cacheDirty = true
	}
	reconfigured := topologyChanged && !publisher.reconciliationPending()
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
