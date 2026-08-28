package main

import (
	"context"
	"encoding/binary"
	"errors"
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

func TestHBACollectorReadDoesNotWaitForRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{Name: "hba0", Temp: 42}}, nil
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
	collector.collectSnapshot = func(context.Context) ([]sensors.HBA, error) { return []sensors.HBA{{Name: "hba0", Temp: 42}}, nil }
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
		readTemperatures: func(_ context.Context, controllers []int) ([]sensors.HBA, error) {
			if len(controllers) != 1 || controllers[0] != 2 {
				t.Fatalf("controllers = %v", controllers)
			}
			return []sensors.HBA{{Name: "hba2", Temp: 51}}, nil
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

func TestExplicitHBABackendSelection(t *testing.T) {
	reader := newHBAReaderForBackend(hbaBackendMPT3CTL)
	if reader.backend.name != "mpt3ctl" {
		t.Fatalf("mpt3ctl backend = %#v", reader.backend)
	}
	reader = newHBAReaderForBackend(hbaBackendStorCLI)
	if reader.backend.name != "storcli" {
		t.Fatalf("storcli backend = %#v", reader.backend)
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
	page5 := make([]byte, 0x20)
	page5[4] = 1
	binary.LittleEndian.PutUint64(page5[0x10:0x18], 0x56c92bf0002e6705)
	if got := parseMPT3SASAddress(page5); got != "56c92bf0002e6705" {
		t.Fatalf("SAS address = %q", got)
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
	if err != nil || len(readings) != 1 || readings[0].Temp != 49 {
		t.Fatalf("got %#v, %v", readings, err)
	}
	bad := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"255"}]}}]}`)
	if _, err := parseStorCLI(bad); err == nil {
		t.Fatal("out-of-range StorCLI temperature accepted")
	}
}

func TestSelectHBAs(t *testing.T) {
	hbas := []sensors.HBA{{Name: "hba0", Temp: 40}, {Name: "hba1", Temp: 50}}
	if got := selectHBAs(hbas, "all"); len(got) != 2 {
		t.Fatalf("all = %#v", got)
	}
	if got := selectHBAs(hbas, "hba1"); len(got) != 1 || got[0].Temp != 50 {
		t.Fatalf("hba1 = %#v", got)
	}
}
