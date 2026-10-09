// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

// This characterizes the current loss of topology when a present controller
// returns no supported probe. It is not a desired thermal-safety invariant:
// an authoritative empty reading removes the probe rather than letting it stale.
func TestStorCLITemperatureAvailabilityTransitionsCurrentBehavior(t *testing.T) {
	const valid = `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"51"},{"Ctrl_Prop":"Ctrl temperature(Degree Celsius)","Value":"43"}]}},{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}]}`
	const missing = `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[]}},{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}]}`
	const recovered = `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"57"},{"Ctrl_Prop":"Ctrl temperature(Degree Celsius)","Value":"45"}]}},{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}]}`
	const removed = `{"Controllers":[{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}]}`
	metadata := map[int]hbaMetadata{0: {id: "sas:0000000000000001"}, 1: {id: "sas:0000000000000002"}}
	report := valid
	var readErr error
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) { return metadata, nil },
		readTemperatures: func(context.Context) (map[int]hbaTemperatures, error) {
			if readErr != nil {
				return nil, readErr
			}
			return parseStorCLI([]byte(report))
		},
	}
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = reader
	root := t.TempDir()
	configRoot, deviceRoot := filepath.Join(root, "config"), filepath.Join(root, "dev")
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", nil)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", nil)
	publisher := &hwmonPublisher{cachePath: filepath.Join(root, "inventory.json")}
	const iocID = "hba:sas:0000000000000001"
	const boardID = "hba:board:sas:0000000000000001"
	const otherID = "hba:sas:0000000000000002"
	for _, phase := range []struct {
		name    string
		report  string
		wantIDs []string
		err     error
	}{
		{name: "valid", report: valid, wantIDs: []string{iocID, boardID, otherID}},
		{name: "present without temperatures", report: missing, wantIDs: []string{otherID}},
		{name: "temperatures recover", report: recovered, wantIDs: []string{iocID, boardID, otherID}},
		{name: "collection error preserves topology", err: errors.New("controller I/O failed"), wantIDs: []string{iocID, boardID, otherID}},
		{name: "collection recovers", report: recovered, wantIDs: []string{iocID, boardID, otherID}},
		{name: "controller removed from inventory", report: removed, wantIDs: []string{otherID}},
		{name: "collection explicitly disabled"},
	} {
		t.Run(phase.name, func(t *testing.T) {
			report, readErr = phase.report, phase.err
			if phase.name == "controller removed from inventory" {
				metadata = map[int]hbaMetadata{1: {id: "sas:0000000000000002"}}
			}
			if phase.name == "collection explicitly disabled" {
				collector = newTestHBACollector(time.Minute, hbaModeDisabled)
			} else {
				collector.refresh(context.Background())
			}
			readings, err := collector.snapshot()
			if (err != nil) != (phase.err != nil) {
				t.Fatalf("snapshot error = %v; want %v", err, phase.err)
			}
			response := sensors.Response{Disks: []sensors.Disk{}, HBAs: readings}
			if err != nil {
				response.HBAError = err.Error()
			}
			_, samples := makeHWMonSamples(response)
			prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", samples)
			for _, sensor := range publisher.hbas.sensors {
				keep := false
				for _, id := range phase.wantIDs {
					keep = keep || id == sensor.id
				}
				if !keep {
					// Ordinary directories need their synthetic attribute removed
					// before rmdir; configfs itself owns its label attribute.
					if err := os.Remove(filepath.Join(configRoot, "hba", hwmonSensorKey("hba", sensor.id), "label")); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, publishErr := publisher.publish(configRoot, deviceRoot, response)
			if (publishErr != nil) != (phase.err != nil) {
				t.Fatalf("publication error = %v; want collection error %v", publishErr, phase.err)
			}
			if len(publisher.hbas.sensors) != len(phase.wantIDs) {
				t.Fatalf("hwmon inventory = %#v; want IDs %v", publisher.hbas.sensors, phase.wantIDs)
			}
			for _, id := range phase.wantIDs {
				if _, err := os.Stat(filepath.Join(configRoot, "hba", hwmonSensorKey("hba", id))); err != nil {
					t.Fatalf("expected HBA probe %q: %v", id, err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(configRoot, "hba"))
			if err != nil || len(entries) != len(phase.wantIDs) {
				t.Fatalf("configfs items = %v, %v; want %d", entries, err, len(phase.wantIDs))
			}
			if phase.err == nil && len(readings) > 0 && readings[len(readings)-1].Temp != 60 {
				t.Fatalf("unaffected controller temperature = %#v; want 60", readings)
			}
			if phase.report == valid || phase.report == recovered {
				wantIOC, wantBoard := 51.0, 43.0
				if phase.report == recovered {
					wantIOC, wantBoard = 57, 45
				}
				if len(readings) != 2 || readings[0].IOCTemp == nil || *readings[0].IOCTemp != wantIOC ||
					readings[0].BoardTemp == nil || *readings[0].BoardTemp != wantBoard {
					t.Fatalf("controller temperatures = %#v; want IOC %v Board %v", readings, wantIOC, wantBoard)
				}
				wantDeviceTemps := map[string]string{iocID: "51000\n", boardID: "43000\n"}
				if phase.report == recovered {
					wantDeviceTemps = map[string]string{iocID: "57000\n", boardID: "45000\n"}
				}
				for id, want := range wantDeviceTemps {
					data, err := os.ReadFile(hwmonTemperatureDevicePath(deviceRoot, id))
					if err != nil || string(data) != want {
						t.Fatalf("temperature device %q = %q, %v; want %q", id, data, err, want)
					}
				}
			}
		})
	}
}

func TestHBAReaderCachesDiscovery(t *testing.T) {
	discoveries := 0
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{2: {id: "sas:1234", model: "SAS3008"}}, nil
		},
		readTemperatures: func(context.Context) (map[int]hbaTemperatures, error) {
			return map[int]hbaTemperatures{2: {ioc: float64Pointer(51)}}, nil
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

func TestHBAReaderRediscoversAndRetriesAfterReadError(t *testing.T) {
	discoveries, reads := 0, 0
	reader := &storCLIReader{
		discoverMetadata: func(context.Context) (map[int]hbaMetadata, error) {
			discoveries++
			return map[int]hbaMetadata{discoveries: {id: fmt.Sprintf("sas:%d", discoveries)}}, nil
		},
		readTemperatures: func(context.Context) (map[int]hbaTemperatures, error) {
			reads++
			if reads == 2 {
				return nil, errors.New("controller changed")
			}
			return map[int]hbaTemperatures{discoveries: {ioc: float64Pointer(51)}}, nil
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
		readTemperatures: func(context.Context) (map[int]hbaTemperatures, error) {
			reads++
			if reads == 2 {
				return map[int]hbaTemperatures{1: {ioc: float64Pointer(52)}}, nil
			}
			return map[int]hbaTemperatures{discoveries - 1: {ioc: float64Pointer(51)}}, nil
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

func TestParseStorCLI(t *testing.T) {
	response := func(properties string) string {
		return `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[` + properties + `]}}]}`
	}
	tests := []struct {
		name      string
		data      string
		wantIOC   *float64
		wantBoard *float64
		wantError string
	}{
		{
			name:    "ROC only",
			data:    response(`{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}`),
			wantIOC: float64Pointer(49),
		},
		{
			name: "ROC with Celcius spelling and Ctrl",
			data: response(`{"Ctrl_Prop":"ROC temperature(Degree Celcius)","Value":"49"},` +
				`{"Ctrl_Prop":"Ctrl temperature(Degree Celsius)","Value":"45"}`),
			wantIOC:   float64Pointer(49),
			wantBoard: float64Pointer(45),
		},
		{
			name:      "Controller name",
			data:      response(`{"Ctrl_Prop":"Controller temperature(Degree Celsius)","Value":"45"}`),
			wantBoard: float64Pointer(45),
		},
		{
			name:    "negative temperature",
			data:    response(`{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"-40"}`),
			wantIOC: float64Pointer(-40),
		},
		{
			name:    "high temperature",
			data:    response(`{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"255"}`),
			wantIOC: float64Pointer(255),
		},
		{
			name: "no supported probe",
			data: response(`{"Ctrl_Prop":"Ambient temperature","Value":"25"}`),
		},
		{name: "malformed JSON", data: `{`, wantError: "parse storcli JSON"},
		{
			name:      "failed status",
			data:      `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`,
			wantError: `status is "Failure"`,
		},
		{
			name:      "invalid temperature",
			data:      response(`{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"broken"}`),
			wantError: `invalid temperature "broken"`,
		},
		{
			name:      "non-finite temperature",
			data:      response(`{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"NaN"}`),
			wantError: `invalid temperature "NaN"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readings, err := parseStorCLI([]byte(test.data))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("got %v, want error containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, found := readings[0]
			if len(readings) != 1 || !found {
				t.Fatalf("temperatures = %#v, want controller 0", readings)
			}
			if !equalOptionalTemperature(got.ioc, test.wantIOC) || !equalOptionalTemperature(got.board, test.wantBoard) {
				t.Fatalf("temperatures = IOC %v, board %v; want IOC %v, board %v", got.ioc, got.board, test.wantIOC, test.wantBoard)
			}
		})
	}
}

func TestBuildHBAReadingsPreservesLegacyProjection(t *testing.T) {
	metadata := map[int]hbaMetadata{0: {id: "sas:1234", model: "SAS3008"}}
	tests := []struct {
		name         string
		temperatures hbaTemperatures
		wantCount    int
		wantLegacy   float64
	}{
		{name: "IOC only", temperatures: hbaTemperatures{ioc: float64Pointer(49)}, wantCount: 1, wantLegacy: 49},
		{name: "board only", temperatures: hbaTemperatures{board: float64Pointer(45)}, wantCount: 1, wantLegacy: 45},
		{name: "IOC and board", temperatures: hbaTemperatures{ioc: float64Pointer(49), board: float64Pointer(45)}, wantCount: 1, wantLegacy: 49},
		{name: "no probes", temperatures: hbaTemperatures{}, wantCount: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildHBAReadings(map[int]hbaTemperatures{0: test.temperatures}, metadata)
			if len(got) != test.wantCount {
				t.Fatalf("readings = %#v, want %d", got, test.wantCount)
			}
			if test.wantCount != 0 && got[0].Temp != test.wantLegacy {
				t.Fatalf("legacy temperature = %v, want %v", got[0].Temp, test.wantLegacy)
			}
		})
	}
}

func TestRunStorCLIPreservesCommandError(t *testing.T) {
	installTestStorCLI(t, `printf 'firmware error\n' >&2; exit 7`)
	_, err := runStorCLI(context.Background(), "discovery", "show")
	if err == nil || !strings.Contains(err.Error(), "storcli discovery: exit status 7: firmware error") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunStorCLITimeout(t *testing.T) {
	installTestStorCLI(t, `exec sleep 10`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runStorCLI(ctx, "temperature", "show")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "storcli temperature timeout") {
		t.Fatalf("error = %v, want storcli timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout returned after %v", elapsed)
	}
}

func TestRunStorCLIBoundsChildHoldingOutputPipe(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	installTestStorCLI(t, "sleep 10 &\nprintf '%s\\n' \"$!\" > '"+pidPath+"'\nexit 0")
	started := time.Now()
	_, err := runStorCLI(context.Background(), "temperature", "show")
	child, findErr := os.FindProcess(readTestPID(t, pidPath))
	if findErr != nil {
		t.Fatal(findErr)
	}
	t.Cleanup(func() { _ = child.Kill() })
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("error = %v, want WaitDelay expiry", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("child holding output pipe delayed return by %v", elapsed)
	}
}

func installTestStorCLI(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "storcli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func readTestPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse PID %q: %v", data, err)
	}
	return pid
}

func TestStorCLIDiscoveryRequiresPCIAddress(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Model":"SAS3008"}}]}`)
	if _, err := parseStorCLIControllers(data); err == nil {
		t.Fatal("StorCLI controller without PCI address accepted")
	}
}

func TestParseStorCLIControllerPCIVariants(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Basics":{"Model":"SAS3008","Serial Number":"ignored","SAS Address":"0x56C92BF0002E6705","PCI Address":"0000:06:10:0"}}},
		{"Command Status":{"Controller":4,"Status":"Success"},"Response Data":{"Product Name":"OEM HBA","PCI Address":"0000:07:00:0"}}
	]}`)
	controllers, err := parseStorCLIControllers(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0: "0000:06:10.0", 4: "0000:07:00.0"}
	if !reflect.DeepEqual(controllers, want) {
		t.Fatalf("controllers = %#v, want %#v", controllers, want)
	}
}

func TestMatchStorCLIControllersToSysfs(t *testing.T) {
	identities := map[string]hbaMetadata{
		"0000:06:10.0": {id: "sas:5000", model: "HBA", pciAddress: "0000:06:10.0"},
	}
	got, err := matchStorCLIControllers(map[int]string{4: "0000:06:10.0"}, identities)
	if err != nil || !reflect.DeepEqual(got, map[int]hbaMetadata{4: identities["0000:06:10.0"]}) {
		t.Fatalf("matched controllers = %#v, %v", got, err)
	}
	if _, err := matchStorCLIControllers(map[int]string{4: "0000:07:00.0"}, identities); err == nil {
		t.Fatal("controller missing from sysfs was accepted")
	}
}

func TestNormalizePCIAddress(t *testing.T) {
	for input, want := range map[string]string{
		"0000:06:10.0": "0000:06:10.0",
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
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"PCI Address":"0000:06:00:0"}},
		{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"PCI Address":"0000:07:00:0"}}
	]}`)
	if _, err := parseStorCLIControllers(discovery); err == nil || !strings.Contains(err.Error(), "appears more than once") {
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
