// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
)

func TestMPT3CommandABI(t *testing.T) {
	request := mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, nil)
	got := makeMPT3Command(3, request, 256, 0x11223344, 0x55667788)
	var want [96]byte
	binary.LittleEndian.PutUint32(want[0:4], 3)
	binary.LittleEndian.PutUint32(want[8:12], 256)
	binary.LittleEndian.PutUint32(want[12:16], mpt3FirmwareTimeout)
	binary.LittleEndian.PutUint64(want[16:24], 0x11223344)
	binary.LittleEndian.PutUint64(want[24:32], 0x55667788)
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
		{"Manufacturing 0", mpi2PageTypeManufacturing, 0, mpi2Manufacturing0Version, []byte{0x00, 0, 0, 0x09}},
		{"Manufacturing 5", mpi2PageTypeManufacturing, 5, mpi2Manufacturing5Version, []byte{0x03, 0, 5, 0x09}},
		{"IO Unit 7", mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, []byte{0x05, 0, 7, 0x00}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := mpt3ConfigRequest(mpi2ConfigPageHeader, test.pageType, test.pageNumber, test.pageVersion, nil)
			if got := request[20:24]; !slices.Equal(got, test.want) {
				t.Fatalf("CONFIG page header = %x, want %x", got, test.want)
			}
		})
	}

	returnedHeader := []byte{0x04, 0x08, 7, mpi2PageTypeIOUnit}
	request := mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, returnedHeader)
	if got := request[20:24]; !slices.Equal(got, returnedHeader) {
		t.Fatalf("CONFIG read header = %x, want returned header %x", got, returnedHeader)
	}
}

func TestMPT3ParsersRejectUndersizedBuffers(t *testing.T) {
	if err := validateMPT3ConfigReply(make([]byte, 0x17), mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err == nil {
		t.Fatal("undersized CONFIG reply accepted")
	}
	if _, err := parseMPT3Temperature(make([]byte, 0x12)); err == nil {
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
		"oversized length": func(reply []byte) { reply[0x02] = 33 },
		"wrong function":   func(reply []byte) { reply[0x03] = 0xff },
		"wrong action":     func(reply []byte) { reply[0x00] = mpi2ConfigPageReadCurrent },
		"failed status": func(reply []byte) {
			binary.LittleEndian.PutUint16(reply[0x0e:0x10], 0x0002)
			binary.LittleEndian.PutUint32(reply[0x10:0x14], 0x12345678)
		},
		"wrong page type":   func(reply []byte) { reply[0x17] = mpi2PageTypeManufacturing },
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

func TestParseMPT3Inventory(t *testing.T) {
	info := make([]byte, 92)
	binary.LittleEndian.PutUint32(info[84:88], 6<<8|16)
	if got, want := parseMPT3PCIAddress(info), "0000:06:10.0"; got != want {
		t.Fatalf("PCI = %q, want %q", got, want)
	}
	page0 := make([]byte, 0x4c)
	copy(page0[0x1c:0x2c], "INSPUR 3008IT  ")
	if got := parseMPT3Model(page0); got != "INSPUR 3008IT" {
		t.Fatalf("model = %q", got)
	}
	copy(page0[0x04:0x14], "LSISAS3008")
	for i := 0x1c; i < 0x2c; i++ {
		page0[i] = 0
	}
	if got := parseMPT3Model(page0); got != "LSISAS3008" {
		t.Fatalf("chip fallback model = %q", got)
	}
	page5 := make([]byte, 0x20)
	page5[4] = 1
	binary.LittleEndian.PutUint64(page5[0x10:0x18], 0x56c92bf0002e6705)
	if got := parseMPT3SASAddress(page5); got != "56c92bf0002e6705" {
		t.Fatalf("SAS address = %q", got)
	}
}

func TestMPT3ReaderKeepsSASIdentityAfterTransientPageFailure(t *testing.T) {
	reader := newMPT3Reader()
	const pci = "0000:06:10.0"
	if got, want := reader.stableID(pci, "56c92bf0002e6705", false), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("initial ID = %q, want %q", got, want)
	}
	if got, want := reader.stableID(pci, "", true), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("ID after Page 5 failure = %q, want %q", got, want)
	}
}

func TestMPT3ReaderDropsSASIdentityAfterPCIDisappears(t *testing.T) {
	reader := newMPT3Reader()
	pci := "0000:06:10.0"

	if got, want := reader.stableID(pci, "56c92bf0002e6705", false), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("initial ID = %q, want %q", got, want)
	}
	reader.retainSASAddressesFor(map[string]struct{}{})

	if got, want := reader.stableID(pci, "", true), "pci:0000:06:10.0"; got != want {
		t.Fatalf("ID after PCI disappearance = %q, want %q", got, want)
	}
	if got, want := reader.stableID(pci, "500605b00abc1234", false), "sas:500605b00abc1234"; got != want {
		t.Fatalf("replacement ID = %q, want %q", got, want)
	}
	if got, want := reader.stableID(pci, "", true), "sas:500605b00abc1234"; got != want {
		t.Fatalf("replacement cached ID = %q, want %q", got, want)
	}
}

func TestMPT3ReaderKeepsSASIdentityForPresentPCI(t *testing.T) {
	reader := newMPT3Reader()
	pci := "0000:06:10.0"

	reader.stableID(pci, "56c92bf0002e6705", false)
	reader.retainSASAddressesFor(map[string]struct{}{pci: {}})

	if got, want := reader.stableID(pci, "", true), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("ID after complete scan = %q, want %q", got, want)
	}
}

func TestMPT3ReaderUsesPCIUntilSASIdentityIsKnown(t *testing.T) {
	reader := newMPT3Reader()
	if got, want := reader.stableID("0000:06:10.0", "", true), "pci:0000:06:10.0"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
}

func TestMPT3ReaderDoesNotReuseCacheForValidPageWithoutSASAddress(t *testing.T) {
	reader := newMPT3Reader()
	const pci = "0000:06:10.0"
	reader.stableID(pci, "56c92bf0002e6705", false)
	if got, want := reader.stableID(pci, "", false), "pci:0000:06:10.0"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
}

func TestParseMPT3Temperature(t *testing.T) {
	for name, test := range map[string]struct {
		raw     int16
		units   byte
		want    float64
		invalid bool
	}{
		"Celsius":         {raw: 42, units: temperatureCelsius, want: 42},
		"Celsius -1":      {raw: -1, units: temperatureCelsius, want: -1},
		"Celsius -40":     {raw: -40, units: temperatureCelsius, want: -40},
		"Celsius 151":     {raw: 151, units: temperatureCelsius, want: 151},
		"Celsius 200":     {raw: 200, units: temperatureCelsius, want: 200},
		"Celsius maximum": {raw: math.MaxInt16, units: temperatureCelsius, want: math.MaxInt16},
		"Fahrenheit 32":   {raw: 32, units: temperatureFahrenheit, want: 0},
		"Fahrenheit -40":  {raw: -40, units: temperatureFahrenheit, want: -40},
		"Fahrenheit 104":  {raw: 104, units: temperatureFahrenheit, want: 40},
		"Fahrenheit 212":  {raw: 212, units: temperatureFahrenheit, want: 100},
		"Fahrenheit high": {raw: 1000, units: temperatureFahrenheit, want: 537.7777777777778},
		"not present":     {units: temperatureNotPresent, invalid: true},
		"unknown units":   {raw: 51, units: 3, invalid: true},
	} {
		t.Run(name, func(t *testing.T) {
			page := make([]byte, 0x17)
			binary.LittleEndian.PutUint16(page[0x10:0x12], uint16(test.raw))
			page[0x12] = test.units
			got, err := parseMPT3Temperature(page)
			if test.invalid && err == nil {
				t.Fatalf("got %v, expected error", got)
			}
			if !test.invalid && (err != nil || math.Abs(got-test.want) > 1e-12) {
				t.Fatalf("got %v, %v; want %v", got, err, test.want)
			}
		})
	}
}
