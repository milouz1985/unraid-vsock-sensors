// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"unsafe"
)

// mpt3ReferenceRequestPageHeader is the literal 28-byte MPI CONFIG PAGE_HEADER
// request for IO Unit Page 7, transcribed from the MPI2 header layout (mpi2_cnfg.h)
// and the in-kernel mpt3sas CONFIG helpers: byte 0 carries the action, byte 3 the
// MPI function, and bytes 20-23 the PageVersion, reserved, PageNumber and
// PageType. It is an independent reference vector that does not reuse
// mpt3ConfigRequest, so a corruption of that helper is detected here.
var mpt3ReferenceRequestPageHeader = [28]byte{
	0:  0x00, // Action = PAGE_HEADER
	3:  0x04, // MPI Function = MPI2_FUNCTION_CONFIG
	20: 0x05, // PageVersion
	21: 0x00, // reserved
	22: 0x07, // PageNumber
	23: 0x00, // PageType = IO Unit
}

// mpt3ReferenceRequestPageReadCurrent is the literal 28-byte MPI CONFIG
// PAGE_READ_CURRENT request built from the firmware-returned header
// {PageVersion 0x05, PageLength 0x04, PageNumber 0x07, PageType 0x00}. Like the
// header request it is an independent reference vector.
var mpt3ReferenceRequestPageReadCurrent = [28]byte{
	0:  0x01, // Action = PAGE_READ_CURRENT
	3:  0x04, // MPI Function = MPI2_FUNCTION_CONFIG
	20: 0x05, // PageVersion
	21: 0x04, // PageLength (in dwords)
	22: 0x07, // PageNumber
	23: 0x00, // PageType = IO Unit
}

func TestMPT3ConfigRequestVectors(t *testing.T) {
	t.Run("PAGE_HEADER uses literal ABI vector", func(t *testing.T) {
		got := mpt3ConfigRequest(0x00, 0x00, 7, 0x05, nil)
		if !slices.Equal(got[:], mpt3ReferenceRequestPageHeader[:]) {
			t.Fatalf("PAGE_HEADER request = %x, want %x", got, mpt3ReferenceRequestPageHeader)
		}
	})
	t.Run("PAGE_READ_CURRENT uses literal ABI vector", func(t *testing.T) {
		header := []byte{0x05, 0x04, 0x07, 0x00}
		got := mpt3ConfigRequest(0x01, 0x00, 7, 0x05, header)
		if !slices.Equal(got[:], mpt3ReferenceRequestPageReadCurrent[:]) {
			t.Fatalf("PAGE_READ_CURRENT request = %x, want %x", got, mpt3ReferenceRequestPageReadCurrent)
		}
	})
}

// TestMPT3CommandIOCTL locks the numeric value of the MPT3COMMAND ioctl request
// word (0xc0484c14) against the literal Linux mpt3sas ABI.
func TestMPT3CommandIOCTL(t *testing.T) {
	if got := mpt3CommandIOCTL; got != uintptr(0xc0484c14) {
		t.Fatalf("mpt3CommandIOCTL = %#x, want 0xc0484c14", got)
	}
}

// TestMPT3CommandABI locks UVSS's Linux/amd64 userspace transcription of
// mpt3_ioctl_command. The expected 96-byte buffer is assembled from the literal
// request vectors in TestMPT3ConfigRequestVectors, not from mpt3ConfigRequest,
// so the two helpers cannot mask each other's corruption.
func TestMPT3CommandABI(t *testing.T) {
	reply, data := new(byte), new(byte)
	replyPointer, dataPointer := unsafe.Pointer(reply), unsafe.Pointer(data)
	got := makeMPT3Command(3, mpt3ReferenceRequestPageReadCurrent, 256, replyPointer, dataPointer)
	var want [96]byte
	binary.LittleEndian.PutUint32(want[0:4], 3)
	binary.LittleEndian.PutUint32(want[12:16], 10)
	binary.LittleEndian.PutUint64(want[16:24], uint64(uintptr(replyPointer)))
	binary.LittleEndian.PutUint64(want[24:32], uint64(uintptr(dataPointer)))
	binary.LittleEndian.PutUint32(want[48:52], 128)
	binary.LittleEndian.PutUint32(want[52:56], 256)
	binary.LittleEndian.PutUint32(want[64:68], 7)
	copy(want[68:96], mpt3ReferenceRequestPageReadCurrent[:])
	if !slices.Equal(got[:], want[:]) {
		t.Fatalf("command buffer = %x, want %x", got, want)
	}
}

func TestMPT3ConfigRequestPageHeader(t *testing.T) {
	request := mpt3ConfigRequest(mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, nil)
	if got := request[20]; got != 0x05 {
		t.Fatalf("IO Unit Page 7 request version = %#02x, want 0x05", got)
	}

	returnedHeader := []byte{0x06, 0xff, 0x07, 0x00}
	request = mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, returnedHeader)
	if got := request[20:24]; !slices.Equal(got, returnedHeader) {
		t.Fatalf("CONFIG read header = %x, want returned header %x", got, returnedHeader)
	}
}

func TestMPT3ParsersRejectUndersizedBuffers(t *testing.T) {
	if err := validateMPT3ConfigReply(make([]byte, 0x17), mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err == nil {
		t.Fatal("undersized CONFIG reply accepted")
	}
	if _, err := parseMPT3Temperatures(make([]byte, 0x16)); err == nil {
		t.Fatal("undersized IO Unit Page 7 accepted")
	}
}

func TestValidateMPT3ConfigReply(t *testing.T) {
	validReply := func() []byte {
		reply := make([]byte, 128)
		reply[0x00] = 0x00
		reply[0x02] = 0x06
		reply[0x03] = 0x04
		reply[0x16] = 7
		reply[0x17] = 0x00
		return reply
	}
	if err := validateMPT3ConfigReply(validReply(), mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err != nil {
		t.Fatalf("valid CONFIG reply rejected: %v", err)
	}

	for name, mutate := range map[string]func([]byte){
		"zero length":      func(reply []byte) { reply[0x02] = 0 },
		"short length":     func(reply []byte) { reply[0x02] = 0x06 - 1 },
		"oversized length": func(reply []byte) { reply[0x02] = 33 },
		"wrong function":   func(reply []byte) { reply[0x03] = 0xff },
		"wrong action":     func(reply []byte) { reply[0x00] = 0x01 },
		"failed status": func(reply []byte) {
			binary.LittleEndian.PutUint16(reply[0x0e:0x10], 0x0002)
			binary.LittleEndian.PutUint32(reply[0x10:0x14], 0x12345678)
		},
		"wrong page type":   func(reply []byte) { reply[0x17] = 0x09 },
		"wrong page number": func(reply []byte) { reply[0x16] = 6 },
	} {
		t.Run(name, func(t *testing.T) {
			reply := validReply()
			mutate(reply)
			if err := validateMPT3ConfigReply(reply, mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err == nil {
				t.Fatal("invalid CONFIG reply accepted")
			}
		})
	}
}

func TestParseMPT3Temperatures(t *testing.T) {
	tests := []struct {
		name       string
		iocRaw     int16
		iocUnits   byte
		boardRaw   int16
		boardUnits byte
		wantIOC    *float64
		wantBoard  *float64
	}{
		{name: "IOC only", iocRaw: 42, iocUnits: 0x02, boardUnits: 0x00, wantIOC: float64Pointer(42)},
		{name: "board only", iocUnits: 0x00, boardRaw: 43, boardUnits: 0x02, wantBoard: float64Pointer(43)},
		{name: "IOC and board", iocRaw: 42, iocUnits: 0x02, boardRaw: 43, boardUnits: 0x02, wantIOC: float64Pointer(42), wantBoard: float64Pointer(43)},
		{name: "no probes", iocUnits: 0x00, boardUnits: 0x00},
		{name: "unknown IOC unit", iocRaw: 51, iocUnits: 0xff, boardRaw: 43, boardUnits: 0x02, wantBoard: float64Pointer(43)},
		{name: "unknown board unit", iocRaw: 42, iocUnits: 0x02, boardRaw: 51, boardUnits: 0xff, wantIOC: float64Pointer(42)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := mpt3TemperaturePage(test.iocRaw, test.iocUnits, test.boardRaw, test.boardUnits)
			got, err := parseMPT3Temperatures(page)
			if err != nil {
				t.Fatal(err)
			}
			if !equalOptionalTemperature(got.ioc, test.wantIOC) || !equalOptionalTemperature(got.board, test.wantBoard) {
				t.Fatalf("temperatures = IOC %v, board %v; want IOC %v, board %v", got.ioc, got.board, test.wantIOC, test.wantBoard)
			}
		})
	}
}

// Use literal firmware bytes at the MPI IO Unit Page 7 offsets. The real
// ioctl boundary remains outside this test; this exercises the parser and
// the reading construction shared with the MPT3 collector.
func TestMPT3TemperatureAvailabilityParsing(t *testing.T) {
	metadata := hbaMetadata{id: "sas:0000000000000001", model: "SAS3008"}
	for _, phase := range []struct {
		name      string
		page      []byte
		available bool
		wantIOC   float64
		wantBoard float64
	}{
		{name: "valid", page: []byte{0x10: 51, 0x12: 2, 0x14: 43, 0x16: 2}, available: true, wantIOC: 51, wantBoard: 43},
		{name: "unsupported units", page: []byte{0x10: 51, 0x12: 0xff, 0x14: 43, 0x16: 0xff}},
		{name: "probes absent", page: []byte{0x10: 51, 0x12: 0, 0x14: 43, 0x16: 0}},
		{name: "recover", page: []byte{0x10: 57, 0x12: 2, 0x14: 45, 0x16: 2}, available: true, wantIOC: 57, wantBoard: 45},
	} {
		t.Run(phase.name, func(t *testing.T) {
			temperatures, err := parseMPT3Temperatures(phase.page)
			if err != nil {
				t.Fatal(err)
			}
			reading, available := makeHBAReading(metadata, temperatures)
			// Current behavior omits a present controller with no usable probe;
			// this documents the risk instead of endorsing deletion as failsafe.
			if available != phase.available {
				t.Fatalf("reading availability = %t; want %t", available, phase.available)
			}
			if available && (reading.ID != "sas:0000000000000001" || reading.Temp != phase.wantIOC ||
				reading.IOCTemp == nil || *reading.IOCTemp != phase.wantIOC || reading.BoardTemp == nil || *reading.BoardTemp != phase.wantBoard) {
				t.Fatalf("reading = %#v; want IOC %v Board %v", reading, phase.wantIOC, phase.wantBoard)
			}
		})
	}
}

func TestParseMPT3TemperaturesIgnoresAdditiveExtensions(t *testing.T) {
	page := mpt3TemperaturePage(42, 0x02, 113, 0x01)
	page = append(page, make([]byte, 997)...)
	got, err := parseMPT3Temperatures(page)
	if err != nil {
		t.Fatal(err)
	}
	if !equalOptionalTemperature(got.ioc, float64Pointer(42)) || !equalOptionalTemperature(got.board, float64Pointer(45)) {
		t.Fatalf("temperatures = IOC %v, board %v; want IOC 42, board 45", got.ioc, got.board)
	}
}

func TestDecodeMPT3Temperature(t *testing.T) {
	tests := []struct {
		name  string
		raw   int16
		units byte
		want  *float64
	}{
		{name: "Celsius", raw: 42, units: 0x02, want: float64Pointer(42)},
		{name: "negative Celsius", raw: -40, units: 0x02, want: float64Pointer(-40)},
		{name: "high Celsius", raw: math.MaxInt16, units: 0x02, want: float64Pointer(math.MaxInt16)},
		{name: "freezing Fahrenheit", raw: 32, units: 0x01, want: float64Pointer(0)},
		{name: "negative Fahrenheit", raw: -40, units: 0x01, want: float64Pointer(-40)},
		{name: "high Fahrenheit", raw: 1000, units: 0x01, want: float64Pointer(537.7777777777778)},
		{name: "not present", units: 0x00},
		{name: "unknown units", raw: 51, units: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := make([]byte, 2)
			binary.LittleEndian.PutUint16(raw, uint16(test.raw))
			got := decodeMPT3Temperature(raw, test.units)
			if !equalOptionalTemperature(got, test.want) {
				t.Fatalf("temperature = %v, want %v", got, test.want)
			}
		})
	}
}

func mpt3TemperaturePage(iocRaw int16, iocUnits byte, boardRaw int16, boardUnits byte) []byte {
	page := make([]byte, 0x17)
	binary.LittleEndian.PutUint16(page[0x10:0x12], uint16(iocRaw))
	page[0x12] = iocUnits
	binary.LittleEndian.PutUint16(page[0x14:0x16], uint16(boardRaw))
	page[0x16] = boardUnits
	return page
}

func equalOptionalTemperature(got, want *float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return math.Abs(*got-*want) <= 1e-12
}
