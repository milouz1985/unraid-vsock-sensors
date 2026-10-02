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
	for _, test := range []struct {
		name                              string
		pageType, pageNumber, pageVersion byte
		want                              []byte
	}{
		{"IO Unit 7", mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, []byte{0x05, 0, 7, 0x00}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := mpt3ConfigRequest(mpi2ConfigPageHeader, test.pageType, test.pageNumber, test.pageVersion, nil)
			if got := request[20:24]; !slices.Equal(got, test.want) {
				t.Fatalf("CONFIG page header = %x, want %x", got, test.want)
			}
		})
	}

	returnedHeader := []byte{0x06, 0xff, 7, mpi2PageTypeIOUnit}
	request := mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, returnedHeader)
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
		reply[0x00] = mpi2ConfigPageHeader
		reply[0x02] = mpi2ConfigReplyDWords
		reply[0x03] = mpi2FunctionConfig
		reply[0x16] = 7
		reply[0x17] = mpi2PageTypeIOUnit
		return reply
	}
	if err := validateMPT3ConfigReply(validReply(), mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err != nil {
		t.Fatalf("valid CONFIG reply rejected: %v", err)
	}

	for name, mutate := range map[string]func([]byte){
		"zero length":      func(reply []byte) { reply[0x02] = 0 },
		"short length":     func(reply []byte) { reply[0x02] = mpi2ConfigReplyDWords - 1 },
		"oversized length": func(reply []byte) { reply[0x02] = 33 },
		"wrong function":   func(reply []byte) { reply[0x03] = 0xff },
		"wrong action":     func(reply []byte) { reply[0x00] = mpi2ConfigPageReadCurrent },
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
		{name: "IOC only", iocRaw: 42, iocUnits: temperatureCelsius, boardUnits: temperatureNotPresent, wantIOC: float64Pointer(42)},
		{name: "board only", iocUnits: temperatureNotPresent, boardRaw: 43, boardUnits: temperatureCelsius, wantBoard: float64Pointer(43)},
		{name: "IOC and board", iocRaw: 42, iocUnits: temperatureCelsius, boardRaw: 43, boardUnits: temperatureCelsius, wantIOC: float64Pointer(42), wantBoard: float64Pointer(43)},
		{name: "no probes", iocUnits: temperatureNotPresent, boardUnits: temperatureNotPresent},
		{name: "unknown IOC unit", iocRaw: 51, iocUnits: 0xff, boardRaw: 43, boardUnits: temperatureCelsius, wantBoard: float64Pointer(43)},
		{name: "unknown board unit", iocRaw: 42, iocUnits: temperatureCelsius, boardRaw: 51, boardUnits: 0xff, wantIOC: float64Pointer(42)},
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

func TestParseMPT3TemperaturesIgnoresAdditiveExtensions(t *testing.T) {
	page := mpt3TemperaturePage(42, temperatureCelsius, 113, temperatureFahrenheit)
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
		{name: "Celsius", raw: 42, units: temperatureCelsius, want: float64Pointer(42)},
		{name: "negative Celsius", raw: -40, units: temperatureCelsius, want: float64Pointer(-40)},
		{name: "high Celsius", raw: math.MaxInt16, units: temperatureCelsius, want: float64Pointer(math.MaxInt16)},
		{name: "freezing Fahrenheit", raw: 32, units: temperatureFahrenheit, want: float64Pointer(0)},
		{name: "negative Fahrenheit", raw: -40, units: temperatureFahrenheit, want: float64Pointer(-40)},
		{name: "high Fahrenheit", raw: 1000, units: temperatureFahrenheit, want: float64Pointer(537.7777777777778)},
		{name: "not present", units: temperatureNotPresent},
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
