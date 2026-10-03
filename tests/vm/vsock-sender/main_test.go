// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
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
}
