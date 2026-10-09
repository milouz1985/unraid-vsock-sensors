// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/binary"
	"math"
	"testing"
)

func FuzzParseMPT3Temperatures(f *testing.F) {
	const minimum = 0x17
	f.Add(mpt3TemperaturePage(42, 0x02, 113, 0x01))

	f.Fuzz(func(t *testing.T, page []byte) {
		temperatures, err := parseMPT3Temperatures(page)
		if len(page) < minimum && err == nil {
			t.Fatalf("short page returned temperatures %#v", temperatures)
		}
		if err != nil {
			return
		}
		checkMPT3FuzzTemperature(t, temperatures.ioc, page[0x10:0x12], page[0x12])
		checkMPT3FuzzTemperature(t, temperatures.board, page[0x14:0x16], page[0x16])
	})
}

func checkMPT3FuzzTemperature(t *testing.T, got *float64, rawBytes []byte, units byte) {
	t.Helper()
	if units != 0x02 && units != 0x01 {
		if got != nil {
			t.Fatalf("unsupported temperature unit 0x%02x returned %v", units, *got)
		}
		return
	}
	if got == nil || math.IsNaN(*got) || math.IsInf(*got, 0) {
		t.Fatalf("supported temperature unit 0x%02x returned %v", units, got)
	}
	raw := int16(binary.LittleEndian.Uint16(rawBytes))
	want := float64(raw)
	if units == 0x01 {
		want = (float64(raw) - 32) * 5 / 9
	}
	if *got != want {
		t.Fatalf("decoded temperature = %v, want %v for raw %d and unit 0x%02x", *got, want, raw, units)
	}
}
