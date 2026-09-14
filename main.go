// SPDX-License-Identifier: GPL-3.0-or-later

// Command unraid-vsock-sensors exports Unraid storage temperatures over AF_VSOCK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"

	"github.com/mdlayher/vsock"
)

const (
	defaultPort            = 990
	defaultPublishInterval = time.Second
	vsockIOTimeout         = 3 * time.Second
)

type stickyErrorLog struct {
	context string
	last    string
}

func (state *stickyErrorLog) update(err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message == state.last {
		return
	}
	state.last = message
	if message == "" {
		log.Printf("%s recovered", state.context)
		return
	}
	log.Printf("%s warning: %s", state.context, message)
}

type snapshotConnection interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

type snapshotDialer func(context.Context) (snapshotConnection, error)

var version = "dev"

func main() {
	// Keep stderr concise: service managers already add timestamps.
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "hwmon":
		err = hwmon(os.Args[2:])
	case "disks":
		err = diskPolicyCommand(os.Args[2:], os.Stdout)
	case "version", "--version":
		fmt.Fprintln(os.Stdout, version)
		return
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  %[1]s serve [options]
  %[1]s hwmon [options]
  %[1]s disks list [options]
  %[1]s disks set --id-base64 ID --policy {auto|include|exclude}
  %[1]s disks validate [options]
  %[1]s disks reset [options]
  %[1]s version

Commands:
  serve                     Push sensor data to the Proxmox host over AF_VSOCK
  hwmon                     Publish fixed storage and HBA hwmon inventories
  disks list                Show disk identity, physical bus and policy as JSON
  disks set                 Save a policy by stable Unraid disk ID
  disks validate            Check the disk policy file
  disks reset               Remove all disk policy overrides
  version                   Print the build version

Serve options:
  --port PORT               AF_VSOCK port (default: 990)
  --hba-mode MODE           HBA collection: enabled or disabled (default: enabled)
  --hba-backend BACKEND     HBA backend: mpt3ctl or storcli (default: mpt3ctl)
  --hba-interval DURATION   Delay between HBA refreshes (default: 15s mpt3ctl, 30s storcli)
  --syslog                  Send service logs to the system logger

Hwmon options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 990)
  --cache PATH              Persistent hwmon inventory cache
                            (default: /var/lib/unraid-vsock-sensors/hwmon-inventory.json)
  --restart-units UNITS     Comma-separated systemd units restarted after a
                            topology change

Examples:
  %[1]s serve --port 990
  %[1]s hwmon --cid 3 --port 990
`, os.Args[0])
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Uint("port", defaultPort, "vsock port")
	hbaModeValue := fs.String("hba-mode", string(hbaModeEnabled), "HBA collection mode")
	hbaBackendValue := fs.String("hba-backend", string(hbaBackendMPT3CTL), "HBA backend")
	hbaIntervalValue := fs.Duration("hba-interval", 0, "delay between HBA refreshes (default: 15s mpt3ctl, 30s storcli)")
	useSyslog := fs.Bool("syslog", false, "send service logs to syslog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve does not accept positional arguments")
	}
	intervalExplicit := false
	fs.Visit(func(option *flag.Flag) {
		if option.Name == "hba-interval" {
			intervalExplicit = true
		}
	})
	if *useSyslog {
		writer, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "unraid-vsock-sensors")
		if err != nil {
			return fmt.Errorf("connect to syslog: %w", err)
		}
		// The writer is intentionally kept for the process lifetime so errors
		// returned by serve and logged by main use the same destination.
		log.SetOutput(writer)
	}
	hbaMode := hbaMode(*hbaModeValue)
	if hbaMode != hbaModeEnabled && hbaMode != hbaModeDisabled {
		return fmt.Errorf("invalid HBA mode %q (expected enabled or disabled)", *hbaModeValue)
	}
	hbaBackend := hbaBackendMode(*hbaBackendValue)
	if hbaBackend != hbaBackendMPT3CTL && hbaBackend != hbaBackendStorCLI {
		return fmt.Errorf("invalid HBA backend %q (expected mpt3ctl or storcli)", *hbaBackendValue)
	}
	hbaInterval, err := resolveHBAInterval(hbaBackend, *hbaIntervalValue, intervalExplicit)
	if err != nil {
		return err
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	disks := newDiskCollector(defaultDiskDataPaths)
	log.Printf("starting unraid-vsock-sensors v%s; pushing to host VSOCK port %d", version, *port)
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()
	refreshSignals := make(chan os.Signal, 1)
	pollSignals := make(chan os.Signal, 1)
	signal.Notify(refreshSignals, unix.SIGUSR1)
	signal.Notify(pollSignals, unix.SIGUSR2)
	defer signal.Stop(refreshSignals)
	defer signal.Stop(pollSignals)
	refreshRequests := make(chan struct{}, 1)
	go forwardDiskRefreshSignals(ctx, refreshSignals, pollSignals, refreshRequests, disks)
	hbas := newConfiguredHBACollector(hbaInterval, hbaMode, hbaBackend)
	// Collection remains independent from publication so a disk or controller
	// command can never block the VSOCK heartbeat.
	go disks.run(ctx, refreshRequests)
	go hbas.run(ctx)
	return publishSnapshots(ctx, uint32(*port), disks, hbas)
}

func resolveHBAInterval(backend hbaBackendMode, interval time.Duration, explicit bool) (time.Duration, error) {
	switch backend {
	case hbaBackendMPT3CTL:
		if !explicit {
			interval = 15 * time.Second
		}
	case hbaBackendStorCLI:
		if !explicit {
			interval = 30 * time.Second
		}
	default:
		return 0, fmt.Errorf("invalid HBA backend %q", backend)
	}
	if interval <= 0 {
		return 0, errors.New("hba-interval must be greater than zero")
	}
	return interval, nil
}

func forwardDiskRefreshSignals(ctx context.Context, manual, poll <-chan os.Signal, refresh chan<- struct{}, disks *diskCollector) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-manual:
			requestDiskRefresh(refresh)
		case <-poll:
			disks.noteEmhttpPoll()
			requestDiskRefresh(refresh)
		}
	}
}

func collectorSnapshot(
	disks *diskCollector,
	collector *hbaCollector,
) sensors.Response {
	diskReadings, diskErr := disks.snapshot()
	hbaReadings, hbaErr := collector.snapshot()
	response := sensors.Response{
		Protocol: sensors.ProtocolVersion, Disks: diskReadings, HBAs: hbaReadings,
	}
	if diskErr != nil {
		response.Error = diskErr.Error()
	}
	if hbaErr != nil {
		response.HBAError = hbaErr.Error()
	}
	return response
}

func publishSnapshots(
	ctx context.Context,
	port uint32,
	disks *diskCollector,
	collector *hbaCollector,
) error {
	dial := func(ctx context.Context) (snapshotConnection, error) {
		return sensors.DialVSOCK(ctx, vsock.Host, port)
	}
	return publishSnapshotsWithDialer(ctx, disks, collector, dial)
}

func publishSnapshotsWithDialer(
	ctx context.Context,
	disks *diskCollector,
	collector *hbaCollector,
	dial snapshotDialer,
) error {
	publishLog := stickyErrorLog{context: "VSOCK publishing"}
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, vsockIOTimeout)
		conn, err := dial(connectCtx)
		cancel()
		if err != nil {
			publishLog.update(err)
			if !waitFor(ctx, defaultPublishInterval) {
				break
			}
			continue
		}
		for ctx.Err() == nil {
			if err = conn.SetWriteDeadline(time.Now().Add(vsockIOTimeout)); err == nil {
				err = sensors.WriteFrame(conn, collectorSnapshot(disks, collector))
			}
			if err != nil {
				_ = conn.Close()
				publishLog.update(err)
				break
			}
			publishLog.update(nil)
			if !waitFor(ctx, defaultPublishInterval) {
				_ = conn.Close()
				return nil
			}
		}
		if ctx.Err() == nil && !waitFor(ctx, defaultPublishInterval) {
			break
		}
	}
	return nil
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
