package main

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestHBACollectorReadDoesNotWaitForRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	collector.collect = func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{Name: "hba0", Temp: 42}}, nil
	}

	done := make(chan struct{})
	go func() {
		collector.refresh(context.Background())
		close(done)
	}()
	<-started

	readDone := make(chan struct{})
	go func() {
		collector.read()
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("read blocked while StorCLI refresh was running")
	}
	close(release)
	<-done
}

func TestHBACollectorFailurePolicy(t *testing.T) {
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	setSuccessfulRefresh := func(temp float64) {
		collector.collect = func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{Name: "hba0", Temp: temp}}, nil
		}
		collector.refresh(context.Background())
	}
	setFailedRefresh := func() {
		collector.collect = func(context.Context) ([]sensors.HBA, error) {
			return nil, errors.New("storcli failed")
		}
		collector.refresh(context.Background())
	}
	checkTemp := func(want float64) {
		t.Helper()
		readings, _ := collector.read()
		if len(readings) != 1 || readings[0].Temp != want {
			t.Fatalf("got readings %#v, want temperature %v", readings, want)
		}
	}

	setSuccessfulRefresh(42)
	checkTemp(42)

	setFailedRefresh()
	readings, err := collector.read()
	if len(readings) != 0 || err == nil {
		t.Fatalf("failed refresh should invalidate readings: %#v, %v", readings, err)
	}

	// A later successful refresh restores the readings.
	setSuccessfulRefresh(50)
	checkTemp(50)
}

func TestHBACollectorAutoIgnoresAbsentHBA(t *testing.T) {
	for name, absent := range map[string]error{
		"StorCLI missing": exec.ErrNotFound,
		"no controller":   errNoHBA,
	} {
		t.Run(name, func(t *testing.T) {
			collector := newHBACollector(time.Minute, hbaModeAuto)
			collector.collect = func(context.Context) ([]sensors.HBA, error) {
				return nil, absent
			}
			collector.refresh(context.Background())

			readings, err := collector.read()
			if len(readings) != 0 || err != nil {
				t.Fatalf("got readings %#v and error %v", readings, err)
			}
		})
	}
}

func TestHBACollectorAutoReportsHBAThatDisappears(t *testing.T) {
	for name, absent := range map[string]error{
		"StorCLI missing": exec.ErrNotFound,
		"no controller":   errNoHBA,
	} {
		t.Run(name, func(t *testing.T) {
			collector := newHBACollector(time.Minute, hbaModeAuto)
			collector.collect = func(context.Context) ([]sensors.HBA, error) {
				return []sensors.HBA{{Name: "hba0", Temp: 42}}, nil
			}
			collector.refresh(context.Background())

			collector.collect = func(context.Context) ([]sensors.HBA, error) {
				return nil, absent
			}
			collector.refresh(context.Background())

			readings, err := collector.read()
			if len(readings) != 0 || !errors.Is(err, absent) {
				t.Fatalf("got readings %#v and error %v, want no readings and %v", readings, err, absent)
			}
		})
	}
}

func TestHBACollectorAutoReportsControllerFailure(t *testing.T) {
	collector := newHBACollector(time.Minute, hbaModeAuto)
	collector.collect = func(context.Context) ([]sensors.HBA, error) {
		return nil, errors.New("controller failed")
	}
	collector.refresh(context.Background())

	if _, err := collector.read(); err == nil || err.Error() != "controller failed" {
		t.Fatalf("got %v", err)
	}
}

func TestHBACollectorEnabledReportsAbsentHBA(t *testing.T) {
	collector := newHBACollector(time.Minute, hbaModeEnabled)
	collector.collect = func(context.Context) ([]sensors.HBA, error) {
		return nil, errNoHBA
	}
	collector.refresh(context.Background())

	if _, err := collector.read(); !errors.Is(err, errNoHBA) {
		t.Fatalf("got %v, want %v", err, errNoHBA)
	}
}

func TestHBACollectorDisabledDoesNotCollect(t *testing.T) {
	collector := newHBACollector(time.Millisecond, hbaModeDisabled)
	collector.collect = func(context.Context) ([]sensors.HBA, error) {
		t.Fatal("disabled collector invoked StorCLI")
		return nil, nil
	}
	collector.run(context.Background())

	readings, err := collector.read()
	if len(readings) != 0 || err != nil {
		t.Fatalf("got readings %#v and error %v", readings, err)
	}
}

func TestParseStorCLI(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"CLI Version":"007.3404.0000.0000 April 18, 2025","Operating system":"Linux 6.18.38-Unraid","Controller":0,"Status":"Success","Description":"None"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}]}},
		{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}
	]}`)
	hbas, err := parseStorCLI(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(hbas) != 2 || hbas[0].Name != "hba0" || hbas[0].Temp != 49 || hbas[1].Temp != 60 {
		t.Fatalf("unexpected HBA readings: %#v", hbas)
	}
	if got := selectHBAs(hbas, "all"); len(got) != 2 {
		t.Fatalf("all selector: %#v", got)
	}
	if got := selectHBAs(hbas, "hba1"); len(got) != 1 || got[0].Temp != 60 {
		t.Fatalf("hba1 selector: %#v", got)
	}
}

func TestParseStorCLIMetadataUsesSerialIdentity(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Product Name":"INSPUR 3008IT","Serial Number":"56c92bf0002e6705","SAS Address":" 56c92bf0002e6705","PCI Address":"00:06:10:00","Bus Number":6,"Device Number":16,"Function Number":0,"Domain ID":0}}]}`)
	metadata, err := parseStorCLIMetadata(data)
	if err != nil {
		t.Fatal(err)
	}
	want := hbaMetadata{
		id:         "serial:56c92bf0002e6705",
		model:      "INSPUR 3008IT",
		pciAddress: "0000:06:10.0",
	}
	if got := metadata[0]; got != want {
		t.Fatalf("metadata = %#v, want %#v", got, want)
	}
}

func TestParseStorCLIMetadataRejectsDuplicateIdentity(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Basics":{"Serial Number":"same"}}},
		{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Basics":{"Serial Number":"same"}}}
	]}`)
	if _, err := parseStorCLIMetadata(data); err == nil {
		t.Fatal("expected duplicate identity error")
	}
}

func TestHBAReaderCachesDiscovery(t *testing.T) {
	discoveries := 0
	reader := &hbaReader{
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{0: {id: "serial:1234", model: "SAS3008"}}, nil
		},
		read: func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{Name: "hba0", Temp: 50}}, nil
		},
	}
	for range 2 {
		readings, err := reader.collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(readings) != 1 || readings[0].ID != "serial:1234" {
			t.Fatalf("unexpected readings: %#v", readings)
		}
	}
	if discoveries != 1 {
		t.Fatalf("discovery count = %d, want 1", discoveries)
	}
}

func TestHBAReaderRejectsUnknownControllerWithoutRediscovery(t *testing.T) {
	discoveries := 0
	reader := &hbaReader{
		discover: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{0: {id: "serial:1234"}}, nil
		},
		read: func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{Name: "hba1", Temp: 50}}, nil
		},
	}

	if _, err := reader.collect(context.Background()); err == nil {
		t.Fatal("an unknown controller should be rejected")
	}
	if discoveries != 1 {
		t.Fatalf("discovery count = %d, want 1", discoveries)
	}
}

func TestStorCLICommandErrorIncludesStderr(t *testing.T) {
	command := exec.Command("sh", "-c", "printf 'controller not found\\n' >&2; exit 1")
	_, commandErr := command.Output()
	if commandErr == nil {
		t.Fatal("command should fail")
	}

	err := storcliCommandError("discovery", commandErr)
	if !errors.Is(err, commandErr) {
		t.Fatalf("wrapped error does not preserve %v", commandErr)
	}
	if got, want := err.Error(), "storcli discovery: exit status 1: controller not found"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestParseStorCLIRejectsUnexpectedOutput(t *testing.T) {
	for name, data := range map[string]string{
		"failed status": `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`,
		"NaN":           storCLIResponseWithTemperature("NaN"),
		"positive Inf":  storCLIResponseWithTemperature("+Inf"),
		"negative Inf":  storCLIResponseWithTemperature("-Inf"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStorCLI([]byte(data)); err == nil {
				t.Fatal("unexpected StorCLI output should be rejected")
			}
		})
	}
}

func TestParseStorCLINoControllers(t *testing.T) {
	if _, err := parseStorCLI([]byte(`{"Controllers":[]}`)); !errors.Is(err, errNoHBA) {
		t.Fatalf("got %v, want %v", err, errNoHBA)
	}
}

func storCLIResponseWithTemperature(temperature string) string {
	return `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"` + temperature + `"}]}}]}`
}
