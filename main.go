// Command unraid-vsock-sensors exports Unraid's cached temperatures over AF_VSOCK.
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
	defaultPort    = 19090
	requestTimeout = 3 * time.Second
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
  --port PORT               AF_VSOCK port (default: 19090)
  --hba-mode MODE           HBA collection: enabled or disabled (default: enabled)
  --storcli-interval DURATION
                            Delay between StorCLI refreshes (default: 30s)

Get options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 19090)
  --json                    Print the complete JSON response

Hwmon options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 19090)
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
  hba:  all or sensor name (for example hba0)

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
	path := fs.String("disks-ini", "/var/local/emhttp/disks.ini", "Unraid live disk state")
	port := fs.Uint("port", defaultPort, "vsock port")
	hbaModeValue := fs.String("hba-mode", string(hbaModeEnabled), "HBA collection mode")
	storcliInterval := fs.Duration("storcli-interval", 30*time.Second, "delay between StorCLI refreshes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storcliInterval <= 0 {
		return errors.New("storcli-interval must be greater than zero")
	}
	hbaMode := hbaMode(*hbaModeValue)
	if hbaMode != hbaModeEnabled && hbaMode != hbaModeDisabled {
		return fmt.Errorf("invalid HBA mode %q (expected enabled or disabled)", *hbaModeValue)
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	listener, err := vsock.Listen(uint32(*port), nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", *port, err)
	}
	defer listener.Close()
	log.Printf("starting unraid-vsock-sensors v%s on vsock port %d", version, *port)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Closing the listener is what releases a blocked Accept during shutdown.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	hbas := newHBACollector(*storcliInterval, hbaMode)
	// StorCLI is refreshed independently so VSOCK requests never wait for the
	// controller command.
	go hbas.run(ctx)
	var clients sync.WaitGroup
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

		// Isolate each client so one blocked connection cannot delay sensor data
		// requested by another host-side consumer.
		clients.Add(1)
		go func() {
			defer clients.Done()
			handle(client, *path, hbas)
		}()
	}
}

func handle(conn net.Conn, path string, collector *hbaCollector) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	// The protocol accepts one fixed command and caps input so an idle or
	// malformed host connection cannot retain unbounded resources.
	line, err := bufio.NewReader(io.LimitReader(conn, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if strings.TrimSpace(line) != "GET" {
		return
	}
	disks, err := readDisks(path)
	r := sensors.Response{
		Version: version, Timestamp: time.Now().UTC(), Disks: disks,
		HBADisabled: collector.mode == hbaModeDisabled,
	}
	if err != nil {
		r.Error = err.Error()
	}
	r.HBAs, err = collector.read()
	if err != nil {
		r.HBAError = err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		log.Printf("write response: %v", err)
	}
}

func get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	all := fs.Bool("json", false, "print the complete JSON response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		if fs.NArg() != 0 {
			return errors.New("--json does not accept a sensor type or selector")
		}
	} else if fs.NArg() != 2 {
		return errors.New("a sensor type and selector are required (for example: disk hdd or hba all)")
	}
	kind := sensorType(fs.Arg(0))
	if !*all && kind != sensorTypeDisk && kind != sensorTypeHBA {
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
	r, err := sensors.Fetch(ctx, uint32(*cid), uint32(*port))
	if err != nil {
		return err
	}
	if *all {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	return writeResponse(os.Stdout, r, kind, fs.Arg(1))
}

func writeResponse(out io.Writer, r sensors.Response, kind sensorType, selector string) error {
	switch kind {
	case sensorTypeHBA:
		var unavailable error
		if r.HBAError != "" {
			unavailable = fmt.Errorf("HBA temperature unavailable: %s", r.HBAError)
		}
		return writeMaxTemperature(out, selectHBAs(r.HBAs, selector), selector, unavailable, func(hba sensors.HBA) float64 {
			return hba.Temp
		})
	case sensorTypeDisk:
		if r.Error != "" {
			return errors.New(r.Error)
		}
		return writeMaxTemperature(out, selectDisks(r.Disks, selector, true), selector, nil, func(disk sensors.Disk) float64 {
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
