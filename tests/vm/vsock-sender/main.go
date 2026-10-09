// SPDX-License-Identifier: GPL-3.0-or-later

// Command vsock-sender sends acceptance snapshots from the nested VM.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mdlayher/vsock"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

type options struct {
	port     uint32
	scenario string
}

func parseOptions(args []string) (options, error) {
	fs := flag.NewFlagSet("vsock-sender", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Uint64("port", 0, "receiver VSOCK port (required)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if err := vsockaddr.ValidatePort(*port); err != nil {
		return options{}, err
	}
	if fs.NArg() != 1 {
		return options{}, errors.New("expected exactly one scenario: valid-1, invalid or valid-2")
	}
	scenario := fs.Arg(0)
	switch scenario {
	case "valid-1", "invalid", "valid-2":
	default:
		return options{}, fmt.Errorf("unknown scenario %q", scenario)
	}
	return options{port: uint32(*port), scenario: scenario}, nil
}

func writeScenario(out io.Writer, scenario string) error {
	temperature := 42.0
	switch scenario {
	case "valid-1":
	case "valid-2":
		temperature = 43
	case "invalid":
		// A complete newline-delimited frame whose JSON is deliberately invalid.
		_, err := io.WriteString(out, "{uvss-invalid-json}\n")
		return err
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}
	return sensors.WriteFrame(out, sensors.Response{
		Protocol: sensors.ProtocolVersion,
		Disks:    []sensors.Disk{{ID: "vsock-e2e", Name: "VSOCK E2E", Temp: temperature}},
		HBAs:     []sensors.HBA{},
	})
}

func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	// The mini init also wraps this process in BusyBox timeout, bounding Dial
	// itself: this version of the VSOCK library has no DialContext API.
	conn, err := vsock.Dial(vsock.Host, opts.port, nil)
	if err != nil {
		return fmt.Errorf("dial host VSOCK port %d: %w", opts.port, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set VSOCK deadline: %w", err)
	}
	if err := writeScenario(conn, opts.scenario); err != nil {
		return fmt.Errorf("write %s: %w", opts.scenario, err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "UVSS-SENDER-ERROR:", err)
		os.Exit(1)
	}
}
