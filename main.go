// Command unraid-vsock-sensors exports Unraid storage temperatures over AF_VSOCK.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"

	"github.com/mdlayher/vsock"
)

const (
	defaultPort          = 990
	requestTimeout       = 3 * time.Second
	maxRequestSize       = 1024
	maxConcurrentClients = 32
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
  serve                     Serve sensor data over AF_VSOCK
  get                       Read sensor data from an AF_VSOCK server
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
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 990)
  --json                    Print the complete JSON response

Hwmon options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 990)
  --interval DURATION       Delay between updates (default: 1s)
  --device PATH             virt-temp control device (default: /dev/virt-temp)
  --cache PATH              Persistent hwmon inventory cache
                            (default: /var/lib/unraid-vsock-sensors/hwmon-inventory.json)
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
  %[1]s get --cid 42 disk hdd
  %[1]s get --cid 42 hba all
  %[1]s get --cid 42 --json
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
	// Keep the former option for upgrades whose service script has not yet been
	// replaced. Both flags update the same value.
	fs.DurationVar(hbaInterval, "storcli-interval", 30*time.Second, "deprecated alias for --hba-interval")
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
	diskFailureGrace := *diskInterval + diskFailureMargin
	disks := newDiskCollector(*disksINIPath, *diskInterval, diskFailureGrace)
	listener, err := vsock.Listen(uint32(*port), nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", *port, err)
	}
	defer listener.Close()
	log.Printf("starting unraid-vsock-sensors v%s on vsock port %d", version, *port)
	log.Printf("disk SMART refresh interval is %s; failure grace is %s", *diskInterval, diskFailureGrace)
	if *diskInterval > maximumRecommendedDiskInterval {
		log.Printf("warning: disk-interval=%s exceeds the recommended maximum of %s; disk temperatures may be too stale for reliable fan control", *diskInterval, maximumRecommendedDiskInterval)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Closing the listener is what releases a blocked Accept during shutdown.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	hbas := newConfiguredHBACollector(*hbaInterval, hbaMode, hbaBackend)
	// Storage temperatures are refreshed independently so VSOCK requests never
	// wait for a disk or controller command.
	go disks.run(ctx)
	go hbas.run(ctx)
	var clients sync.WaitGroup
	clientSlots := make(chan struct{}, maxConcurrentClients)
	defer clients.Wait()
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		// Only the Proxmox host (the well-known vsock CID 2) may query the server.
		// RemoteAddr returns the generic net.Addr interface; this type assertion
		// verifies that it contains the concrete *vsock.Addr needed to read the CID.
		peer, ok := client.RemoteAddr().(*vsock.Addr)
		if !ok || peer.ContextID != vsock.Host {
			_ = client.Close()
			continue
		}
		select {
		case clientSlots <- struct{}{}:
		default:
			_ = client.Close()
			continue
		}

		// Isolate each client so one blocked connection cannot delay sensor data
		// requested by another host-side consumer.
		clients.Add(1)
		go func() {
			defer clients.Done()
			defer func() { <-clientSlots }()
			handle(client, disks, hbas)
		}()
	}
}

func handle(conn net.Conn, disks *diskCollector, collector *hbaCollector) {
	handleWithTimeout(conn, disks, collector, requestTimeout)
}

func handleWithTimeout(conn net.Conn, disks *diskCollector, collector *hbaCollector, timeout time.Duration) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	// The protocol accepts one fixed command and caps input so an idle or
	// malformed host connection cannot retain unbounded resources.
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestSize+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if len(line) > maxRequestSize || strings.TrimSpace(line) != "GET" {
		return
	}
	diskReadings, err := disks.read()
	response := sensors.Response{
		Version: version, Timestamp: time.Now().UTC(), Disks: diskReadings,
		HBADisabled: collector.mode == hbaModeDisabled,
	}
	if err != nil {
		response.Error = err.Error()
	}
	response.HBAs, err = collector.read()
	if err != nil {
		response.HBAError = err.Error()
	}
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		log.Printf("write response: %v", err)
	}
}

func get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
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
	if err := vsockaddr.ValidateCID(uint64(*cid)); err != nil {
		return err
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	response, err := sensors.Fetch(ctx, uint32(*cid), uint32(*port))
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
		var unavailable error
		if response.HBAError != "" {
			unavailable = fmt.Errorf("HBA temperature unavailable: %s", response.HBAError)
		}
		return writeMaxTemperature(out, selectHBAs(response.HBAs, selector), selector, unavailable, func(hba sensors.HBA) float64 {
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
