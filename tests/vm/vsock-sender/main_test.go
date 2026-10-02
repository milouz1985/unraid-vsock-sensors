// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"unraid-vsock-sensors/internal/sensors"
)

func TestWriteScenario(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scenario    string
		temperature float64
	}{
		{"valid-1", 42}, {"valid-2", 43},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeScenario(&out, tc.scenario); err != nil {
				t.Fatal(err)
			}
			reader := sensors.NewFrameReader(&out)
			got, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			want := sensors.Response{Protocol: sensors.ProtocolVersion,
				Disks: []sensors.Disk{{ID: "vsock-e2e", Name: "VSOCK E2E", Temp: tc.temperature}}, HBAs: []sensors.HBA{}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot = %#v, want %#v", got, want)
			}
			if _, err := reader.Read(); !errors.Is(err, io.EOF) {
				t.Fatalf("extra frame: %v", err)
			}
		})
	}
	var out bytes.Buffer
	if err := writeScenario(&out, "invalid"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{uvss-invalid-json}\n" {
		t.Fatalf("invalid frame = %q", out.String())
	}
	_, err := sensors.NewFrameReader(&out).Read()
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("invalid frame must produce JSON syntax error: %v", err)
	}
	if err := writeScenario(io.Discard, "unknown"); err == nil {
		t.Fatal("accepted unknown scenario")
	}
}

func TestParseOptions(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"valid-1", "invalid", "valid-2"} {
		t.Run(scenario, func(t *testing.T) {
			got, err := parseOptions([]string{"--port", "990", scenario})
			if err != nil || got != (options{port: 990, scenario: scenario}) {
				t.Fatalf("options = %v, error = %v", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"empty", nil}, {"missing port", []string{"valid-1"}},
		{"zero port", []string{"--port", "0", "valid-1"}},
		{"negative port", []string{"--port", "-1", "valid-1"}},
		{"reserved port", []string{"--port", "4294967295", "valid-1"}},
		{"overflow port", []string{"--port", "4294967296", "valid-1"}},
		{"missing scenario", []string{"--port", "990"}},
		{"unknown scenario", []string{"--port", "990", "unknown"}},
		{"extra argument", []string{"--port", "990", "valid-1", "extra"}},
		{"unknown flag", []string{"--unknown", "990", "valid-1"}},
		{"invalid number", []string{"--port", "abc", "valid-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOptions(tc.args); err == nil {
				t.Errorf("accepted arguments %q", tc.args)
			}
		})
	}
}
