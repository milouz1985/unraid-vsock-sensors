// unraid-vsock-sensors exports Unraid's cached disk temperatures over AF_VSOCK.
package main

import (
	"bufio"
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

	"github.com/mdlayher/vsock"
)

const defaultPort = 19090

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
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s serve [options] | get [options] SELECTOR\n", os.Args[0])
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("disks-ini", "/var/local/emhttp/disks.ini", "Unraid live disk state")
	port := fs.Uint("port", defaultPort, "vsock port")
	storcliCache := fs.Duration("storcli-cache", 30*time.Second, "minimum interval between storcli calls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	listener, err := vsock.Listen(uint32(*port), nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", *port, err)
	}
	defer listener.Close()
	log.Printf("listening on vsock port %d", *port)
	hbas := &hbaCollector{maxAge: *storcliCache}
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

		go handle(client, *path, hbas)
	}
}

func handle(conn net.Conn, path string, collector *hbaCollector) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if strings.TrimSpace(line) != "GET" {
		return
	}
	disks, err := readDisks(path)
	r := response{Timestamp: time.Now().UTC(), Disks: disks}
	if err != nil {
		r.Error = err.Error()
	}
	r.HBAs, err = collector.read()
	if err != nil {
		r.HBAError = err.Error()
	}
	_ = json.NewEncoder(conn).Encode(r)
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
	r, err := fetch(uint32(*cid), uint32(*port))
	if err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if *all {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	selector := fs.Arg(0)
	if strings.EqualFold(selector, "hba") || strings.HasPrefix(strings.ToLower(selector), "hba") {
		selected := selectHBAs(r.HBAs, selector)
		if len(selected) == 0 {
			if r.HBAError != "" {
				return fmt.Errorf("HBA temperature unavailable: %s", r.HBAError)
			}
			return fmt.Errorf("no available temperature for selector %q", selector)
		}
		max := selected[0].Temp
		for _, sensor := range selected[1:] {
			if sensor.Temp > max {
				max = sensor.Temp
			}
		}
		fmt.Println(strconv.FormatFloat(max, 'f', -1, 64))
		return nil
	}
	selected := selectDisks(r.Disks, selector)
	if len(selected) == 0 {
		return fmt.Errorf("no available temperature for selector %q", fs.Arg(0))
	}
	max := selected[0].Temp
	for _, d := range selected[1:] {
		if d.Temp > max {
			max = d.Temp
		}
	}
	fmt.Println(strconv.FormatFloat(max, 'f', -1, 64))
	return nil
}

func fetch(cid, port uint32) (response, error) {
	var out response
	conn, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return out, fmt.Errorf("connect to vsock %d:%d: %w", cid, port, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "GET\n"); err != nil {
		return out, err
	}
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
