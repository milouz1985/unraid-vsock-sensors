// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
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

func TestHBACollectorSnapshotDoesNotWaitForRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	})
	done := make(chan struct{})
	go func() { collector.refresh(context.Background()); close(done) }()
	<-started
	readDone := make(chan struct{})
	go func() { _, _ = collector.snapshot(); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cached read blocked during HBA refresh")
	}
	close(release)
	<-done
}

func TestHBACollectorFailureInvalidatesSnapshot(t *testing.T) {
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	})
	collector.refresh(context.Background())
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return nil, errors.New("failed")
	})
	collector.refresh(context.Background())
	readings, err := collector.snapshot()
	if len(readings) != 0 || err == nil {
		t.Fatalf("failed refresh returned %#v, %v", readings, err)
	}
}

func TestHBACollectorExpiresBlockedRefreshAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		collector := newTestHBACollector(time.Minute, hbaModeEnabled)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
		})
		collector.refresh(context.Background())
		// The normal interval between collections must not expire the cache.
		time.Sleep(collector.interval)
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 42 {
			t.Fatalf("between collections: readings=%v err=%v", readings, err)
		}

		release := make(chan struct{})
		defer close(release)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			<-release // A synchronous ioctl can keep waiting after its context expires.
			return []sensors.HBA{{ID: "sas:1234", Temp: 43}}, nil
		})
		go collector.refresh(context.Background())
		synctest.Wait()
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 42 {
			t.Fatalf("during collection: readings=%v err=%v", readings, err)
		}

		time.Sleep(hbaCollectionTimeout)
		synctest.Wait()
		if readings, err := collector.snapshot(); len(readings) != 0 || err == nil {
			t.Fatalf("blocked past deadline: readings=%v err=%v", readings, err)
		}

		release <- struct{}{}
		synctest.Wait()
		if readings, err := collector.snapshot(); len(readings) != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late successful collection: readings=%v err=%v", readings, err)
		}

		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 44}}, nil
		})
		collector.refresh(context.Background())
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 44 {
			t.Fatalf("after recovery: readings=%v err=%v", readings, err)
		}
	})
}

func TestBlockedHBACollectionDoesNotStopSnapshotPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		collector := newTestHBACollector(time.Second, hbaModeEnabled)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
		})
		collector.refresh(context.Background())

		release := make(chan struct{})
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			<-release // Simulate a synchronous ioctl ignoring its expired context.
			return nil, context.DeadlineExceeded
		})
		go collector.refresh(context.Background())
		synctest.Wait()
		time.Sleep(collector.interval + hbaCollectionTimeout)
		synctest.Wait()

		frames := capturePublishedSnapshots(
			t,
			newDiskCollector(diskDataPaths{}),
			collector,
			2,
		)
		for index, frame := range frames {
			if frame.HBAError == "" {
				t.Fatalf("frame %d did not publish the blocked HBA collection error: %#v", index, frame)
			}
		}

		close(release)
		synctest.Wait()
	})
}

func TestHBACollectorDisabledDoesNotCollect(t *testing.T) {
	collector := newTestHBACollector(time.Millisecond, hbaModeDisabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		t.Fatal("disabled collector performed collection")
		return nil, nil
	})
	collector.run(context.Background())
	readings, err := collector.snapshot()
	if readings == nil || len(readings) != 0 || err != nil {
		t.Fatalf("got %#v, %v", readings, err)
	}
}

func TestHBAReaderCachesDiscovery(t *testing.T) {
	discoveries := 0
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{2: {id: "sas:1234", model: "SAS3008"}}, nil
		},
		readTemperatures: func(context.Context) (map[int]float64, error) {
			return map[int]float64{2: 51}, nil
		},
	}
	for range 2 {
		readings, err := reader.collect(context.Background())
		if err != nil || len(readings) != 1 || readings[0].ID != "sas:1234" {
			t.Fatalf("readings %#v, error %v", readings, err)
		}
	}
	if discoveries != 1 {
		t.Fatalf("discovery count = %d", discoveries)
	}
}

func TestHBAStableIDPrefersSASAcrossBackends(t *testing.T) {
	if got, want := hbaStableID("0x56:C9:2B:F0:00:2E:67:05", "0000:06:10.0", "SERIAL"), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
	if got, want := hbaStableID("", "0000:06:10.0", "SERIAL"), "pci:0000:06:10.0"; got != want {
		t.Fatalf("PCI fallback ID = %q, want %q", got, want)
	}
	if got, want := hbaStableID("", "", "SERIAL"), "serial:serial"; got != want {
		t.Fatalf("serial fallback ID = %q, want %q", got, want)
	}
}

func TestHBAReaderRediscoversAndRetriesAfterReadError(t *testing.T) {
	discoveries, reads := 0, 0
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{discoveries: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(context.Context) (map[int]float64, error) {
			reads++
			if reads == 2 {
				return nil, errors.New("controller changed")
			}
			return map[int]float64{discoveries: 51}, nil
		},
	}
	if _, err := reader.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	readings, err := reader.collect(context.Background())
	if err != nil || discoveries != 2 || reads != 3 || readings[0].ID != "sas:2" {
		t.Fatalf("discoveries=%d reads=%d readings=%#v err=%v", discoveries, reads, readings, err)
	}
}

func TestHBAReaderRediscoversOnControllerSetMismatch(t *testing.T) {
	discoveries, reads := 0, 0
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{discoveries - 1: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(context.Context) (map[int]float64, error) {
			reads++
			if reads == 2 {
				return map[int]float64{1: 52}, nil
			}
			return map[int]float64{discoveries - 1: 51}, nil
		},
	}
	if _, err := reader.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	readings, err := reader.collect(context.Background())
	if err != nil || discoveries != 2 || reads != 3 || readings[0].ID != "sas:2" {
		t.Fatalf("discoveries=%d reads=%d readings=%#v err=%v", discoveries, reads, readings, err)
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
		"Celsius":         {raw: 51, units: temperatureCelsius, want: 51},
		"Fahrenheit":      {raw: 122, units: temperatureFahrenheit, want: 50},
		"not present":     {units: temperatureNotPresent, invalid: true},
		"unknown units":   {raw: 51, units: 3, invalid: true},
		"negative signed": {raw: -20, units: temperatureCelsius, invalid: true},
		"out of range":    {raw: 255, units: temperatureCelsius, invalid: true},
	} {
		t.Run(name, func(t *testing.T) {
			page := make([]byte, 0x17)
			binary.LittleEndian.PutUint16(page[0x10:0x12], uint16(test.raw))
			page[0x12] = test.units
			got, err := parseMPT3Temperature(page)
			if test.invalid && err == nil {
				t.Fatalf("got %v, expected error", got)
			}
			if !test.invalid && (err != nil || got != test.want) {
				t.Fatalf("got %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

func TestParseStorCLI(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}]}}]}`)
	readings, err := parseStorCLI(data)
	if err != nil || len(readings) != 1 || readings[0] != 49 {
		t.Fatalf("got %#v, %v", readings, err)
	}
	bad := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"255"}]}}]}`)
	if _, err := parseStorCLI(bad); err == nil {
		t.Fatal("out-of-range StorCLI temperature accepted")
	}
}

func TestParseStorCLIRejectsUnexpectedOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{name: "malformed JSON", data: `{`, want: "parse storcli JSON"},
		{name: "failed status", data: `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`, want: `status is "Failure"`},
		{name: "missing temperature", data: `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[]}}]}`, want: "has no ROC temperature"},
		{name: "not a number", data: storCLIResponseWithTemperature("broken"), want: `invalid temperature "broken"`},
		{name: "NaN", data: storCLIResponseWithTemperature("NaN"), want: `invalid temperature "NaN"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseStorCLI([]byte(test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func storCLIResponseWithTemperature(temperature string) string {
	return `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"` + temperature + `"}]}}]}`
}

func TestStorCLIDiscoveryRequiresStableIdentity(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Model":"SAS3008"}}]}`)
	if _, err := parseStorCLIMetadata(data); err == nil {
		t.Fatal("StorCLI controller without stable identity accepted")
	}
}

func TestParseStorCLIMetadataVariants(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Basics":{"Model":"SAS3008","Serial Number":"ignored","SAS Address":"0x56C92BF0002E6705","PCI Address":"0000:06:10:0"}}},
		{"Command Status":{"Controller":4,"Status":"Success"},"Response Data":{"Product Name":"OEM HBA","Serial Number":"SERIAL-4"}}
	]}`)
	metadata, err := parseStorCLIMetadata(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := metadata[0], (hbaMetadata{id: "sas:56c92bf0002e6705", model: "SAS3008", pciAddress: "0000:06:10.0"}); got != want {
		t.Fatalf("Basics metadata = %#v, want %#v", got, want)
	}
	if got, want := metadata[4], (hbaMetadata{id: "serial:serial-4", model: "OEM HBA"}); got != want {
		t.Fatalf("flat metadata = %#v, want %#v", got, want)
	}
}

func TestNormalizePCIAddress(t *testing.T) {
	for input, want := range map[string]string{
		"0000:06:10:0": "0000:06:10.0",
		"0:6:10:0":     "0000:06:10.0",
		"0000:06:20:0": "",
		"invalid":      "",
	} {
		if got := normalizePCIAddress(input); got != want {
			t.Errorf("normalizePCIAddress(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStorCLIParsersRejectDuplicateControllerNumbers(t *testing.T) {
	discovery := []byte(`{"Controllers":[
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"SAS Address":"1"}},
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"SAS Address":"2"}}
	]}`)
	if _, err := parseStorCLIMetadata(discovery); err == nil || !strings.Contains(err.Error(), "appears more than once") {
		t.Fatalf("duplicate discovery controllers returned %v", err)
	}

	temperatures := []byte(`{"Controllers":[
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}]}},
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"50"}]}}
	]}`)
	if _, err := parseStorCLI(temperatures); err == nil || !strings.Contains(err.Error(), "appears more than once") {
		t.Fatalf("duplicate temperature controllers returned %v", err)
	}
}
