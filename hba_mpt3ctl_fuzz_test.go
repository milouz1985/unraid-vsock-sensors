// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func addMPT3BoundarySeeds(f *testing.F, minimum int) {
	f.Helper()
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add(make([]byte, minimum-1))
	f.Add(make([]byte, minimum))
	f.Add(make([]byte, minimum+16))
	f.Add(bytes.Repeat([]byte{0xff}, minimum+16))
}

func mpt3TemperatureSeed(raw int16, units byte) []byte {
	page := make([]byte, 0x17)
	binary.LittleEndian.PutUint16(page[0x10:0x12], uint16(raw))
	page[0x12] = units
	return page
}

func FuzzParseMPT3Temperature(f *testing.F) {
	const minimum = 0x13
	addMPT3BoundarySeeds(f, minimum)

	f.Add(mpt3TemperatureSeed(51, temperatureCelsius))
	f.Add(mpt3TemperatureSeed(122, temperatureFahrenheit))
	f.Add(mpt3TemperatureSeed(0, temperatureNotPresent))
	f.Add(mpt3TemperatureSeed(51, 0xff))
	for _, raw := range []int16{math.MinInt16, -32767, -40, -1, 0, 32, 100, 150, 151, 200, math.MaxInt16} {
		f.Add(mpt3TemperatureSeed(raw, temperatureCelsius))
		f.Add(mpt3TemperatureSeed(raw, temperatureFahrenheit))
	}

	f.Fuzz(func(t *testing.T, page []byte) {
		temperature, err := parseMPT3Temperature(page)
		if len(page) < minimum && err == nil {
			t.Fatalf("short page returned temperature %v", temperature)
		}
		if len(page) >= minimum && page[0x12] != temperatureCelsius && page[0x12] != temperatureFahrenheit && err == nil {
			t.Fatalf("unsupported temperature unit 0x%02x was accepted", page[0x12])
		}
		if err != nil {
			return
		}
		if math.IsNaN(temperature) || math.IsInf(temperature, 0) {
			t.Fatalf("non-finite temperature: %v", temperature)
		}

		raw := int16(binary.LittleEndian.Uint16(page[0x10:0x12]))
		want := float64(raw)
		if page[0x12] == temperatureFahrenheit {
			want = (float64(raw) - 32) * 5 / 9
		}
		if temperature != want {
			t.Fatalf("decoded temperature = %v, want %v for raw %d and unit 0x%02x", temperature, want, raw, page[0x12])
		}
	})
}

func validMPT3ConfigReplySeed() []byte {
	reply := make([]byte, mpt3ReplyBufferSize)
	reply[0x00] = mpi2ConfigPageHeader
	reply[0x02] = mpi2ConfigReplyDWords
	reply[0x03] = mpi2FunctionConfig
	reply[0x16] = 7
	reply[0x17] = mpi2PageTypeIOUnit
	return reply
}

func FuzzValidateMPT3ConfigReply(f *testing.F) {
	addMPT3BoundarySeeds(f, mpi2ConfigReplySize)
	f.Add(validMPT3ConfigReplySeed())

	f.Fuzz(func(t *testing.T, reply []byte) {
		err := validateMPT3ConfigReply(reply, mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7)
		if len(reply) < mpi2ConfigReplySize {
			if err == nil {
				t.Fatal("undersized MPI CONFIG reply was accepted")
			}
			return
		}
		if err != nil {
			return
		}

		if reply[0x02] != mpi2ConfigReplyDWords ||
			reply[0x03] != mpi2FunctionConfig ||
			reply[0x00] != mpi2ConfigPageHeader ||
			binary.LittleEndian.Uint16(reply[0x0e:0x10])&mpi2IOCStatusMask != 0 ||
			reply[0x17]&0x0f != mpi2PageTypeIOUnit ||
			reply[0x16] != 7 {
			t.Fatalf("invalid MPI CONFIG reply was accepted: %x", reply)
		}
	})
}
