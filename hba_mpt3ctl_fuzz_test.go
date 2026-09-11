// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"regexp"
	"strings"
	"testing"
)

var (
	mpt3PCIAddressPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)
	mpt3SASAddressPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
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

func FuzzParseMPT3PCIAddress(f *testing.F) {
	const minimum = 92
	addMPT3BoundarySeeds(f, minimum)

	valid := make([]byte, minimum)
	binary.LittleEndian.PutUint32(valid[84:88], 0xab<<8|5<<5|0x1c)
	binary.LittleEndian.PutUint32(valid[88:92], 0x1234)
	f.Add(valid)

	invalidBus := make([]byte, minimum)
	binary.LittleEndian.PutUint32(invalidBus[84:88], 0x100<<8)
	f.Add(invalidBus)
	invalidSegment := make([]byte, minimum)
	binary.LittleEndian.PutUint32(invalidSegment[88:92], 0x10000)
	f.Add(invalidSegment)

	f.Fuzz(func(t *testing.T, info []byte) {
		got := parseMPT3PCIAddress(info)
		if len(info) < minimum && got != "" {
			t.Fatalf("short IOC info returned PCI address %q", got)
		}
		if len(info) >= minimum {
			pci := binary.LittleEndian.Uint32(info[84:88])
			segment := binary.LittleEndian.Uint32(info[88:92])
			if (segment > 0xffff || pci>>8 > 0xff) && got != "" {
				t.Fatalf("out-of-range PCI address fields returned %q", got)
			}
		}
		if got != "" && !mpt3PCIAddressPattern.MatchString(got) {
			t.Fatalf("PCI address %q does not match dddd:bb:dd.f", got)
		}
		if again := parseMPT3PCIAddress(info); again != got {
			t.Fatalf("non-deterministic PCI address: first %q, then %q", got, again)
		}
	})
}

func FuzzParseMPT3Model(f *testing.F) {
	const minimum = 0x2c
	addMPT3BoundarySeeds(f, minimum)

	boardName := make([]byte, minimum)
	copy(boardName[0x1c:0x2c], "INSPUR 3008IT  ")
	f.Add(boardName)

	chipFallback := make([]byte, minimum)
	copy(chipFallback[0x04:0x14], "LSISAS3008")
	f.Add(chipFallback)

	nonASCIIBoard := make([]byte, minimum)
	copy(nonASCIIBoard[0x04:0x14], "LSISAS3008")
	copy(nonASCIIBoard[0x1c:0x2c], "BOARD")
	nonASCIIBoard[0x21] = 0x80
	f.Add(nonASCIIBoard)

	ffPadding := bytes.Repeat([]byte{0xff}, minimum)
	copy(ffPadding[0x1c:0x2c], "MPT3 BOARD")
	f.Add(ffPadding)

	spaceAndNULPadding := bytes.Repeat([]byte{' '}, minimum)
	copy(spaceAndNULPadding[0x1c:0x2c], []byte{' ', 'M', 'P', 'T', '3', ' ', 'B', 'O', 'A', 'R', 'D', ' ', 0, 'X'})
	f.Add(spaceAndNULPadding)

	f.Fuzz(func(t *testing.T, page []byte) {
		model := parseMPT3Model(page)
		if model == "" {
			return
		}
		if strings.TrimSpace(model) != model {
			t.Fatalf("model has surrounding whitespace: %q", model)
		}
		for _, character := range []byte(model) {
			if character < 32 || character >= 127 {
				t.Fatalf("model contains non-printable ASCII byte 0x%02x: %q", character, model)
			}
		}
		if strings.IndexByte(model, 0) >= 0 {
			t.Fatalf("model contains a NUL byte: %q", model)
		}
	})
}

func FuzzParseMPT3SASAddress(f *testing.F) {
	const minimum = 0x10
	addMPT3BoundarySeeds(f, minimum)

	noPHY := make([]byte, minimum)
	noPHY[4] = 0
	f.Add(noPHY)
	oneMissingPHY := make([]byte, minimum)
	oneMissingPHY[4] = 1
	f.Add(oneMissingPHY)
	tooManyPHYs := make([]byte, 0x18)
	tooManyPHYs[4] = 2
	f.Add(tooManyPHYs)
	maximumPHYs := make([]byte, minimum)
	maximumPHYs[4] = 255
	f.Add(maximumPHYs)
	truncatedPHY := make([]byte, 0x14)
	truncatedPHY[4] = 1
	f.Add(truncatedPHY)

	zeroAddress := make([]byte, 0x20)
	zeroAddress[4] = 1
	f.Add(zeroAddress)

	validAddress := make([]byte, 0x20)
	validAddress[4] = 1
	binary.LittleEndian.PutUint64(validAddress[0x10:0x18], 0x56c92bf0002e6705)
	f.Add(validAddress)

	secondAddress := make([]byte, 0x28)
	secondAddress[4] = 2
	binary.LittleEndian.PutUint64(secondAddress[0x20:0x28], 0x500605b00abcdef0)
	f.Add(secondAddress)

	f.Fuzz(func(t *testing.T, page []byte) {
		address := parseMPT3SASAddress(page)
		if address == "" {
			return
		}
		if !mpt3SASAddressPattern.MatchString(address) {
			t.Fatalf("invalid SAS address %q", address)
		}
		if address == "0000000000000000" {
			t.Fatal("zero SAS address was returned")
		}
	})
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

		messageBytes := int(reply[0x02]) * 4
		if messageBytes < mpi2ConfigReplySize || messageBytes > len(reply) ||
			reply[0x03] != mpi2FunctionConfig ||
			reply[0x00] != mpi2ConfigPageHeader ||
			binary.LittleEndian.Uint16(reply[0x0e:0x10])&mpi2IOCStatusMask != 0 ||
			reply[0x17]&0x0f != mpi2PageTypeIOUnit ||
			reply[0x16] != 7 {
			t.Fatalf("invalid MPI CONFIG reply was accepted: %x", reply)
		}
	})
}
