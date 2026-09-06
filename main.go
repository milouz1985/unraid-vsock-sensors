// Command unraid-vsock-sensors exports Unraid storage temperatures over AF_VSOCK.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"

	"github.com/mdlayher/vsock"
)

const (
	defaultPort            = 990
	defaultPublishInterval = time.Second
	requestTimeout         = 3 * time.Second
	maxRequestSize         = 1024
)

var version = "dev"

type sensorType string

const (
	sensorTypeDisk sensorType = "disk"
	sensorTypeHBA  sensorType = "hba"
)

func main() {
	// Keep stderr concise: service managers already add timestamps, while sensor
	// values are written separately to stdout for cmd-based consumers.
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "get":
		err = get(os.Args[2:])
	case "hwmon":
		err = hwmon(os.Args[2:])
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
  %[1]s get [options] TYPE SELECTOR
  %[1]s get [options] --json
  %[1]s hwmon [options]
  %[1]s version

Commands:
  serve                     Push sensor data to the Proxmox host over AF_VSOCK
  get                       Read the latest snapshot from the host daemon
  hwmon                     Publish fixed storage and HBA hwmon inventories
  version                   Print the build version

Serve options:
  --disks-ini PATH          Unraid disk state (default: /var/local/emhttp/disks.ini)
  --disk-interval DURATION  Delay between disk SMART refreshes (default: 30s)
  --port PORT               AF_VSOCK port (default: 990)
  --hba-mode MODE           HBA collection: enabled or disabled (default: enabled)
  --hba-backend BACKEND     HBA backend: mpt3ctl or storcli (default: mpt3ctl)
  --hba-interval DURATION   Delay between HBA temperature refreshes (default: 30s)
  --syslog                  Send service logs to the system logger

Get options:
  --socket PATH             Host daemon socket
                            (default: /run/unraid-vsock-sensors/sensors.sock)
  --json                    Print the complete JSON response

Hwmon options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 990)
  --device PATH             virt-temp control device (default: /dev/virt-temp)
  --cache PATH              Persistent hwmon inventory cache
                            (default: /var/lib/unraid-vsock-sensors/hwmon-inventory.json)
  --socket PATH             Local socket served to get
                            (default: /run/unraid-vsock-sensors/sensors.sock)
  --restart-units UNITS     Comma-separated systemd units restarted after a
                            topology change

Sensor types:
  disk                      Select disks, pools, or disk groups
  hba                       Select HBA temperature sensors

Selectors:
  disk: hdd, ssd, nvme, all, disk name, or device name
  hba:  all or stable controller ID (for example sas:500605b00abc1234)

Examples:
  %[1]s serve --port 990
  %[1]s get disk hdd
  %[1]s get hba all
  %[1]s get --json
`, os.Args[0])
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	disksINIPath := fs.String("disks-ini", "/var/local/emhttp/disks.ini", "Unraid live disk state")
	diskInterval := fs.Duration("disk-interval", defaultDiskInterval, "delay between disk SMART refreshes")
	port := fs.Uint("port", defaultPort, "vsock port")
	hbaModeValue := fs.String("hba-mode", string(hbaModeEnabled), "HBA collection mode")
	hbaBackendValue := fs.String("hba-backend", string(hbaBackendMPT3CTL), "HBA backend")
	hbaInterval := fs.Duration("hba-interval", 30*time.Second, "delay between HBA temperature refreshes")
	useSyslog := fs.Bool("syslog", false, "send service logs to syslog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve does not accept positional arguments")
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
	if *hbaInterval <= 0 {
		return errors.New("hba-interval must be greater than zero")
	}
	if *diskInterval <= 0 {
		return errors.New("disk-interval must be greater than zero")
	}
	hbaMode := hbaMode(*hbaModeValue)
	if hbaMode != hbaModeEnabled && hbaMode != hbaModeDisabled {
		return fmt.Errorf("invalid HBA mode %q (expected enabled or disabled)", *hbaModeValue)
	}
	hbaBackend := hbaBackendMode(*hbaBackendValue)
	if hbaBackend != hbaBackendMPT3CTL && hbaBackend != hbaBackendStorCLI {
		return fmt.Errorf("invalid HBA backend %q (expected mpt3ctl or storcli)", *hbaBackendValue)
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	disks := newDiskCollector(*disksINIPath, *diskInterval)
	log.Printf("starting unraid-vsock-sensors v%s; pushing to host VSOCK port %d", version, *port)
	log.Printf("disk SMART refresh interval is %s; failure grace is %s", disks.interval, disks.grace)
	if *diskInterval > maximumRecommendedDiskInterval {
		log.Printf("warning: disk-interval=%s exceeds the recommended maximum of %s; disk temperatures may be too stale for reliable fan control", *diskInterval, maximumRecommendedDiskInterval)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hbas := newConfiguredHBACollector(*hbaInterval, hbaMode, hbaBackend)
	// Collection remains independent from publication so a disk or controller
	// command can never block the VSOCK heartbeat.
	go disks.run(ctx)
	go hbas.run(ctx)
	return publishSnapshots(ctx, uint32(*port), disks, hbas)
}

func collectorMessages(
	disks *diskCollector,
	collector *hbaCollector,
) (sensors.Message, uint64, sensors.Message, uint64, sensors.Message) {
	now := time.Now().UTC()
	diskReadings, diskErr, diskRevision, diskValidFor := disks.snapshot()
	hbaReadings, hbaErr, hbaRevision, hbaValidFor := collector.snapshot()
	diskResponse := sensors.Response{Version: version, Timestamp: now, Disks: diskReadings}
	if diskErr != nil {
		diskResponse.Error = diskErr.Error()
	}
	hbaResponse := sensors.Response{
		Version: version, Timestamp: now, HBAs: hbaReadings,
		HBADisabled: collector.mode == hbaModeDisabled,
	}
	if hbaErr != nil {
		hbaResponse.HBAError = hbaErr.Error()
	}
	heartbeat := sensors.Message{
		Type:         sensors.MessageHeartbeat,
		DiskValidFor: diskValidFor,
		HBAValidFor:  hbaValidFor,
		Response:     sensors.Response{Version: version, Timestamp: now},
	}
	return sensors.Message{
			Type: sensors.MessageDisks, DiskValidFor: diskValidFor, Response: diskResponse,
		}, diskRevision, sensors.Message{
			Type: sensors.MessageHBAs, HBAValidFor: hbaValidFor, Response: hbaResponse,
		}, hbaRevision, heartbeat
}

func publishSnapshots(
	ctx context.Context,
	port uint32,
	disks *diskCollector,
	collector *hbaCollector,
) error {
	lastError := ""
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		conn, err := sensors.DialVSOCK(connectCtx, vsock.Host, port)
		cancel()
		if err != nil {
			message := err.Error()
			if message != lastError {
				log.Printf("VSOCK publish warning: %s", message)
				lastError = message
			}
			if !waitFor(ctx, defaultPublishInterval) {
				break
			}
			continue
		}
		if lastError != "" {
			log.Printf("VSOCK publishing recovered")
			lastError = ""
		}
		var lastDisks sensors.Message
		var diskRevision, hbaRevision uint64
		disksSent, hbasSent := false, false
		for ctx.Err() == nil {
			diskMessage, nextDiskRevision, hbaMessage, nextHBARevision, heartbeat :=
				collectorMessages(disks, collector)
			if !disksSent || nextDiskRevision != diskRevision || !sameDiskMessage(lastDisks, diskMessage) {
				err = writeStreamMessage(conn, diskMessage)
				if err == nil {
					lastDisks, diskRevision, disksSent = diskMessage, nextDiskRevision, true
				}
			}
			if err == nil && (!hbasSent || nextHBARevision != hbaRevision) {
				err = writeStreamMessage(conn, hbaMessage)
				if err == nil {
					hbaRevision, hbasSent = nextHBARevision, true
				}
			}
			if err == nil {
				err = writeStreamMessage(conn, heartbeat)
			}
			if err != nil {
				_ = conn.Close()
				message := err.Error()
				if message != lastError {
					log.Printf("VSOCK publish warning: %s", message)
					lastError = message
				}
				break
			}
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

func writeStreamMessage(conn interface {
	SetWriteDeadline(time.Time) error
	io.Writer
}, message sensors.Message) error {
	if err := conn.SetWriteDeadline(time.Now().Add(requestTimeout)); err != nil {
		return err
	}
	return sensors.WriteFrame(conn, message)
}

func sameDiskMessage(left, right sensors.Message) bool {
	return left.Error == right.Error && slices.Equal(left.Disks, right.Disks)
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

func get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	socketPath := fs.String("socket", defaultSnapshotSocket, "host daemon socket")
	printJSON := fs.Bool("json", false, "print the complete JSON response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *printJSON {
		if fs.NArg() != 0 {
			return errors.New("--json does not accept a sensor type or selector")
		}
	} else if fs.NArg() != 2 {
		return errors.New("a sensor type and selector are required (for example: disk hdd or hba all)")
	}
	kind := sensorType(fs.Arg(0))
	if !*printJSON && kind != sensorTypeDisk && kind != sensorTypeHBA {
		return fmt.Errorf("unknown sensor type %q (expected disk or hba)", kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	response, err := sensors.FetchUnix(ctx, *socketPath)
	if err != nil {
		return err
	}
	if *printJSON {
		return json.NewEncoder(os.Stdout).Encode(response)
	}
	return writeResponse(os.Stdout, response, kind, fs.Arg(1))
}

func writeResponse(out io.Writer, response sensors.Response, kind sensorType, selector string) error {
	switch kind {
	case sensorTypeHBA:
		if response.HBAError != "" {
			return fmt.Errorf("HBA temperature unavailable: %s", response.HBAError)
		}
		return writeMaxTemperature(out, selectHBAs(response.HBAs, selector), selector, nil, func(hba sensors.HBA) float64 {
			return hba.Temp
		})
	case sensorTypeDisk:
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return writeMaxTemperature(out, selectDisks(response.Disks, selector), selector, nil, func(disk sensors.Disk) float64 {
			if disk.Unavailable {
				return hwmonFailsafeTemp
			}
			return disk.Temp
		})
	default:
		return fmt.Errorf("unknown sensor type %q (expected disk or hba)", kind)
	}
}

func writeMaxTemperature[T any](
	out io.Writer,
	items []T,
	selector string,
	unavailable error,
	temperature func(T) float64,
) error {
	if len(items) == 0 {
		if unavailable != nil {
			return unavailable
		}
		return fmt.Errorf("no available temperature for selector %q", selector)
	}
	_, err := fmt.Fprintln(out, strconv.FormatFloat(
		sensors.MaxTemperature(items, temperature), 'f', -1, 64,
	))
	return err
}
