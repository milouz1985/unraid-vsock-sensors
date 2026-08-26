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
