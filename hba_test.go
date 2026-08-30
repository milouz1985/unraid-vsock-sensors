package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"unraid-vsock-sensors/internal/sensors"
)

func TestMPT3CommandABI(t *testing.T) {
	var command mpt3Command
	if got, want := unsafe.Offsetof(command.Request), uintptr(68); got != want {
		t.Fatalf("MPI request offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(command), uintptr(96); got != want {
		t.Fatalf("command buffer size = %d, want %d", got, want)
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

func TestHBACollectorReadDoesNotWaitForRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	}
	done := make(chan struct{})
	go func() { collector.refresh(context.Background()); close(done) }()
	<-started
	readDone := make(chan struct{})
	go func() { _, _ = collector.read(); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cached read blocked during HBA refresh")
	}
	close(release)
	<-done
}

func TestHBACollectorFailureInvalidatesSnapshot(t *testing.T) {
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) { return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil }
	collector.refresh(context.Background())
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) { return nil, errors.New("failed") }
	collector.refresh(context.Background())
	readings, err := collector.read()
	if len(readings) != 0 || err == nil {
		t.Fatalf("failed refresh returned %#v, %v", readings, err)
	}
}

func TestHBACollectorDisabledDoesNotCollect(t *testing.T) {
	collector := newHBACollector(time.Millisecond, hbaModeDisabled)
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) {
		t.Fatal("disabled collector performed collection")
		return nil, nil
	}
	collector.run(context.Background())
	readings, err := collector.read()
	if len(readings) != 0 || err != nil {
		t.Fatalf("got %#v, %v", readings, err)
	}
}

func TestHBAReaderCachesDiscovery(t *testing.T) {
	discoveries := 0
	reader := &hbaReader{backend: hbaBackend{
		name: "test",
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{2: {id: "sas:1234", model: "SAS3008"}}, nil
		},
		readTemperatures: func(_ context.Context, controllers []int) (map[int]float64, error) {
			if len(controllers) != 1 || controllers[0] != 2 {
				t.Fatalf("controllers = %v", controllers)
			}
			return map[int]float64{2: 51}, nil
		},
	}}
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

func TestHBAReaderRefreshesChangedTopology(t *testing.T) {
	discoveries := 0
	topology := "one"
	reader := &hbaReader{backend: hbaBackend{
		name: "test", topology: func() (string, error) { return topology, nil },
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{0: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(context.Context, []int) (map[int]float64, error) {
			return map[int]float64{0: 50}, nil
		},
	}}
	first, err := reader.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	topology = "two"
	second, err := reader.collect(context.Background())
	if err != nil || discoveries != 2 || first[0].ID == second[0].ID {
		t.Fatalf("discoveries=%d first=%#v second=%#v err=%v", discoveries, first, second, err)
	}
}

func TestHBAReaderRediscoversAndRetriesAfterReadError(t *testing.T) {
	discoveries, reads := 0, 0
	reader := &hbaReader{backend: hbaBackend{
		name: "test",
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{discoveries: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(_ context.Context, controllers []int) (map[int]float64, error) {
			reads++
			if reads == 2 {
				return nil, errors.New("controller changed")
			}
			return map[int]float64{controllers[0]: 51}, nil
		},
	}}
	if _, err := reader.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	readings, err := reader.collect(context.Background())
	if err != nil || discoveries != 2 || reads != 3 || readings[0].ID != "sas:2" {
		t.Fatalf("discoveries=%d reads=%d readings=%#v err=%v", discoveries, reads, readings, err)
	}
}

func TestExplicitHBABackendSelection(t *testing.T) {
	reader := newHBAReaderForBackend(hbaBackendMPT3CTL)
	if reader.backend.name != "mpt3ctl" || reader.backend.collect == nil {
		t.Fatalf("mpt3ctl backend = %#v", reader.backend)
	}
	reader = newHBAReaderForBackend(hbaBackendStorCLI)
	if reader.backend.name != "storcli" || reader.backend.collect != nil || reader.backend.topology == nil {
		t.Fatalf("storcli backend = %#v", reader.backend)
	}
}

func TestHBAReaderRediscoversOnControllerSetMismatch(t *testing.T) {
	discoveries, reads := 0, 0
	reader := &hbaReader{backend: hbaBackend{
		name: "test",
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{discoveries - 1: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(_ context.Context, controllers []int) (map[int]float64, error) {
			reads++
			if reads == 2 {
				return map[int]float64{1: 52}, nil
			}
			return map[int]float64{controllers[0]: 51}, nil
		},
	}}
	if _, err := reader.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	readings, err := reader.collect(context.Background())
	if err != nil || discoveries != 2 || reads != 3 || readings[0].ID != "sas:2" {
		t.Fatalf("discoveries=%d reads=%d readings=%#v err=%v", discoveries, reads, readings, err)
	}
}

func TestReadHBATopologyAt(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "devices", "0000:06:00.0")
	if err := os.MkdirAll(device, 0755); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(root, "host2")
	if err := os.MkdirAll(host, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "proc_name"), []byte("mpt3sas\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(host, "device")); err != nil {
		t.Fatal(err)
	}
	first, err := readHBATopologyAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "sas_address"), []byte("0x5000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := readHBATopologyAt(root)
	if err != nil || first == second {
		t.Fatalf("first=%q second=%q err=%v", first, second, err)
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

func TestStorCLIDiscoveryRequiresStableIdentity(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Model":"SAS3008"}}]}`)
	if _, err := parseStorCLIMetadata(data); err == nil {
		t.Fatal("StorCLI controller without stable identity accepted")
	}
}

func TestSelectHBAs(t *testing.T) {
	hbas := []sensors.HBA{{ID: "sas:1234", Temp: 40}, {ID: "pci:0000:06:10.0", Temp: 50}}
	if got := selectHBAs(hbas, "all"); len(got) != 2 {
		t.Fatalf("all = %#v", got)
	}
	if got := selectHBAs(hbas, "PCI:0000:06:10.0"); len(got) != 1 || got[0].Temp != 50 {
		t.Fatalf("PCI selector = %#v", got)
	}
}
