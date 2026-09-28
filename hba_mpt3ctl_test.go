// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

func TestValidateMPT3ConfigPageHeader(t *testing.T) {
	validHeader := func() []byte {
		return []byte{mpi2IOUnit7Version, mpi2IOUnit7DWords, 7, mpi2PageTypeIOUnit}
	}
	if err := validateMPT3ConfigPageHeader(validHeader(), mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, mpi2IOUnit7DWords); err != nil {
		t.Fatalf("valid IO Unit Page 7 header rejected: %v", err)
	}

	for name, mutate := range map[string]func([]byte){
		"wrong version":     func(header []byte) { header[0]-- },
		"wrong length":      func(header []byte) { header[1]-- },
		"wrong page number": func(header []byte) { header[2]-- },
		"wrong page type":   func(header []byte) { header[3] = 0x09 },
	} {
		t.Run(name, func(t *testing.T) {
			header := validHeader()
			mutate(header)
			if err := validateMPT3ConfigPageHeader(header, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, mpi2IOUnit7DWords); err == nil {
				t.Fatal("invalid IO Unit Page 7 header accepted")
			}
		})
	}
	if err := validateMPT3ConfigPageHeader([]byte{1, 2, 3}, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, mpi2IOUnit7DWords); err == nil {
		t.Fatal("short IO Unit Page 7 header accepted")
	}
}

func TestValidateMPT3ConfigPageData(t *testing.T) {
	header := [4]byte{mpi2IOUnit7Version, mpi2IOUnit7DWords, 7, mpi2PageTypeIOUnit}
	page := make([]byte, int(mpi2IOUnit7DWords)*4)
	copy(page, header[:])
	if err := validateMPT3ConfigPageData(page, header); err != nil {
		t.Fatalf("valid IO Unit Page 7 data rejected: %v", err)
	}
	page[0]--
	if err := validateMPT3ConfigPageData(page, header); err == nil {
		t.Fatal("page data with mismatched header accepted")
	}
	if err := validateMPT3ConfigPageData([]byte{1, 2, 3}, header); err == nil {
		t.Fatal("page data with short header accepted")
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

func mpt3IOCInfoForTest(segment, bus, device, function uint32) []byte {
	info := make([]byte, 92)
	binary.LittleEndian.PutUint32(info[84:88], bus<<8|function<<5|device)
	binary.LittleEndian.PutUint32(info[88:92], segment)
	return info
}

func TestMatchMPT3ControllersScansFullU8RangeAndStopsWhenMatched(t *testing.T) {
	identities := map[string]hbaMetadata{
		"0000:06:10.0": {id: "sas:56c92bf0002e6705", pciAddress: "0000:06:10.0", driver: "mpt3sas"},
		"0000:07:00.0": {id: "pci:0000:07:00.0", pciAddress: "0000:07:00.0", driver: "megaraid_sas"},
	}
	const wantedIOC = 200
	calls := 0
	controllers, err := matchMPT3Controllers(context.Background(), identities, func(ioc int) ([]byte, error) {
		calls++
		if ioc == wantedIOC {
			return mpt3IOCInfoForTest(0, 6, 16, 0), nil
		}
		return nil, unix.ENODEV
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != wantedIOC+1 {
		t.Fatalf("IOCINFO calls = %d, want %d", calls, wantedIOC+1)
	}
	if len(controllers) != 1 || controllers[0].ioc != wantedIOC || controllers[0].metadata.id != identities["0000:06:10.0"].id {
		t.Fatalf("controllers = %#v", controllers)
	}
}

func TestMatchMPT3ControllersBoundsMissingIOCScan(t *testing.T) {
	identities := map[string]hbaMetadata{
		"0000:06:10.0": {id: "pci:0000:06:10.0", pciAddress: "0000:06:10.0", driver: "mpt3sas"},
	}
	calls := 0
	_, err := matchMPT3Controllers(context.Background(), identities, func(int) ([]byte, error) {
		calls++
		return nil, unix.ENODEV
	})
	if err == nil || !strings.Contains(err.Error(), "matched 0 of 1") {
		t.Fatalf("missing IOC returned %v", err)
	}
	if calls != mpt3IOCSlots {
		t.Fatalf("IOCINFO calls = %d, want %d", calls, mpt3IOCSlots)
	}
}

func TestParseMPT3Inventory(t *testing.T) {
	info := make([]byte, 92)
	binary.LittleEndian.PutUint32(info[84:88], 6<<8|16)
	if got, want := parseMPT3PCIAddress(info), "0000:06:10.0"; got != want {
		t.Fatalf("PCI = %q, want %q", got, want)
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
