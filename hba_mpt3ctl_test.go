// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"
)

// TestMPT3CommandABI locks UVSS's Linux/amd64 userspace transcription of
// mpt3_ioctl_command. It does not inspect the ABI of the loaded kernel, so an
// incompatible field-layout change that preserves the structure size remains
// a residual risk.
func TestMPT3CommandABI(t *testing.T) {
	request := mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, nil)
	reply, data := new(byte), new(byte)
	replyPointer, dataPointer := unsafe.Pointer(reply), unsafe.Pointer(data)
	got := makeMPT3Command(3, request, 256, replyPointer, dataPointer)
	var want [96]byte
	binary.LittleEndian.PutUint32(want[0:4], 3)
	binary.LittleEndian.PutUint32(want[12:16], mpt3FirmwareTimeout)
	binary.LittleEndian.PutUint64(want[16:24], uint64(uintptr(replyPointer)))
	binary.LittleEndian.PutUint64(want[24:32], uint64(uintptr(dataPointer)))
	binary.LittleEndian.PutUint32(want[48:52], mpt3ReplyBufferSize)
	binary.LittleEndian.PutUint32(want[52:56], 256)
	binary.LittleEndian.PutUint32(want[64:68], 7)
	copy(want[68:96], request[:])
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

func TestMPT3DiscoveryUsesSysfsIOC(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scsi_host")
	addFakeSCSIHost(t, root, "host2", "mpt3sas", "0000:06:10.0", "0x5001", "HBA", fakeSysfsValue("200"))
	addFakeSCSIHost(t, root, "host3", "megaraid_sas", "0000:07:00.0", "0x5002", "RAID", nil)

	controllers, err := (&mpt3Reader{sysfsRoot: root}).discoverControllers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(controllers) != 1 || controllers[0].ioc != 200 || controllers[0].metadata.id != "sas:0000000000005001" {
		t.Fatalf("controllers = %#v", controllers)
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
