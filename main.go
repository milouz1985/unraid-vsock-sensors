// SPDX-License-Identifier: GPL-3.0-or-later

// Command unraid-vsock-sensors exports Unraid storage temperatures over AF_VSOCK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultPort            = 990
	defaultPublishInterval = time.Second
)

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
	case "diagnostics":
		err = diagnosticsCommand(os.Args[2:], os.Stdout)
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
  %[1]s disks refresh
  %[1]s diagnostics
  %[1]s version

Commands:
  serve                    Push sensor data to the Proxmox host over AF_VSOCK
  hwmon                    Publish fixed storage and HBA hwmon inventories
  disks list               Show disk identity, physical bus and policy as JSON
  disks set                Save a policy by stable Unraid disk ID
  disks validate           Check the disk policy file
  disks reset              Remove all disk policy overrides
  disks refresh            Request a disk collection refresh from the daemon
  diagnostics              Print the daemon's read-only runtime state as JSON
  version                  Print the build version

The disks commands are clients of the daemon control socket; the daemon must be
running. All commands accept --control-socket to override the socket path.

Serve options:
  --port PORT               AF_VSOCK port (default: 990)
  --hba-mode MODE           HBA collection: enabled or disabled (default: enabled)
  --hba-backend BACKEND     HBA backend: mpt3ctl or storcli (default: mpt3ctl)
  --hba-interval DURATION   Delay between HBA refreshes (default: 15s mpt3ctl, 30s storcli)
  --syslog                  Send service logs to the system logger
  --control-socket PATH     Local control Unix socket (default: /run/unraid-vsock-sensors/control.sock)

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
	controlSocket := fs.String("control-socket", defaultControlSocketPath, "local control Unix socket")
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
	if *useSyslog {
		writer, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "unraid-vsock-sensors")
		if err != nil {
			return fmt.Errorf("connect to syslog: %w", err)
		}
		// The writer is intentionally kept for the process lifetime so errors
		// returned by serve and logged by main use the same destination.
		log.SetOutput(writer)
	}
	disks := newDiskCollector(defaultDiskDataPaths)
	log.Printf("starting unraid-vsock-sensors v%s; pushing to host VSOCK port %d", version, *port)
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()
	refreshRequests := make(chan struct{}, 1)
	// The emhttpd poll_attributes heartbeat is frequent and carries no data, so
	// it is delivered out of band (SIGUSR2) instead of spawning a process per
	// event over the control socket. It records the heartbeat and requests a
	// disk refresh. Manual refresh stays an explicit control-socket command.
	pollSignals := make(chan os.Signal, 1)
	signal.Notify(pollSignals, unix.SIGUSR2)
	defer signal.Stop(pollSignals)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pollSignals:
				disks.noteEmhttpPoll()
				requestDiskRefresh(refreshRequests)
			}
		}
	}()
	hbas := newConfiguredHBACollector(hbaInterval, hbaMode, hbaBackend)
	service := newServiceState(uint32(*port))
	// The control socket is the entry point for explicit control operations:
	// disk policy mutations and manual refresh.
	control := newControlServer(*controlSocket, refreshRequests,
		defaultDiskPolicyFile, defaultDisksINIPath, defaultDevsINIPath, defaultSysBlockRoot)
	// Initial control-socket setup is required for a valid daemon start. After
	// that, the control server supervises and recreates its own listener so a
	// local control-plane failure cannot stop thermal collection or VSOCK
	// publication.
	if err := control.start(); err != nil {
		return err
	}
	// Shut the control plane down synchronously so the Unix socket is removed
	// before serve returns, regardless of how the daemon is being stopped.
	defer control.stop()
	// Collection remains independent from publication so a disk or controller
	// command can never block the VSOCK heartbeat.
	go disks.run(ctx, refreshRequests)
	go hbas.run(ctx)
	go runDiagnostics(ctx, defaultDiagnosticsPath, service, disks, hbas)
	return publishSnapshots(ctx, uint32(*port), disks, hbas, service)
}
