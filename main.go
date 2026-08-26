// unraid-vsock-sensors exports Unraid's cached disk temperatures over AF_VSOCK.
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
	"strconv"
	"strings"
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
  %[1]s get [options] SELECTOR
  %[1]s get [options] --json
  %[1]s version

Commands:
  serve                     Serve sensor data over AF_VSOCK
  get                       Read sensor data from an AF_VSOCK server
  version                   Print the build version

Serve options:
  --disks-ini PATH          Unraid disk state (default: /var/local/emhttp/disks.ini)
  --port PORT               AF_VSOCK port (default: 19090)
  --storcli-cache DURATION  Interval between StorCLI refreshes (default: 30s)

Get options:
  --cid CID                 Guest AF_VSOCK CID (default: 3)
  --port PORT               AF_VSOCK port (default: 19090)
  --json                    Print the complete JSON response

Selectors:
  hdd, ssd, nvme, all, hba, hba0, disk name, or device name

Examples:
  %[1]s serve --port 990
  %[1]s get --cid 42 hdd
  %[1]s get --cid 42 --json
`, os.Args[0])
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("disks-ini", "/var/local/emhttp/disks.ini", "Unraid live disk state")
	port := fs.Uint("port", defaultPort, "vsock port")
	storcliCache := fs.Duration("storcli-cache", 30*time.Second, "interval between storcli refreshes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storcliCache <= 0 {
		return errors.New("storcli-cache must be greater than zero")
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hbas := newHBACollector(*storcliCache)
	// The `go` keyword starts run in a new goroutine, a lightweight concurrent
	// task managed by Go. This lets the server accept requests immediately while
	// StorCLI is refreshed independently in the background.
	go hbas.run(ctx)
	for {
		client, err := listener.Accept()
		if err != nil {
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

		// Start one goroutine per accepted connection so a slow client does not
		// prevent the accept loop from receiving and serving other clients.
		go handle(client, *path, hbas)
	}
}

func handle(conn net.Conn, path string, collector *hbaCollector) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if strings.TrimSpace(line) != "GET" {
		return
	}
	disks, err := readDisks(path)
	r := sensors.Response{Version: version, Timestamp: time.Now().UTC(), Disks: disks}
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
	if fs.NArg() != 1 && !*all {
		return errors.New("a selector is required (hdd, nvme, all, name, or device)")
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
	return writeResponse(os.Stdout, r, fs.Arg(0), *all)
}

func writeResponse(out io.Writer, r sensors.Response, selector string, all bool) error {
	if all {
		return json.NewEncoder(out).Encode(r)
	}
	if strings.HasPrefix(strings.ToLower(selector), "hba") {
		selected := selectHBAs(r.HBAs, selector)
		if len(selected) == 0 {
			if r.HBAError != "" {
				return fmt.Errorf("HBA temperature unavailable: %s", r.HBAError)
			}
			return fmt.Errorf("no available temperature for selector %q", selector)
		}
		fmt.Fprintln(out, strconv.FormatFloat(sensors.MaxTemperature(selected, func(hba sensors.HBA) float64 {
			return hba.Temp
		}), 'f', -1, 64))
		return nil
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	selected := selectDisks(r.Disks, selector)
	if len(selected) == 0 {
		return fmt.Errorf("no available temperature for selector %q", selector)
	}
	fmt.Fprintln(out, strconv.FormatFloat(sensors.MaxTemperature(selected, func(disk sensors.Disk) float64 {
		return disk.Temp
	}), 'f', -1, 64))
	return nil
}
