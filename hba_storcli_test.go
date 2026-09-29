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
)

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
	for _, spelling := range []string{"Celsius", "Celcius"} {
		t.Run(spelling, func(t *testing.T) {
			property := "ROC temperature(Degree " + spelling + ")"
			data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"` + property + `","Value":"49"}]}}]}`)
			readings, err := parseStorCLI(data)
			if err != nil || len(readings) != 1 || readings[0].ioc == nil || *readings[0].ioc != 49 || readings[0].board != nil {
				t.Fatalf("got %#v, %v", readings, err)
			}
		})
	}
	for raw, want := range map[string]float64{"0": 0, "-40": -40, "151": 151, "255": 255} {
		readings, err := parseStorCLI([]byte(storCLIResponseWithTemperature(raw)))
		if err != nil || readings[0].ioc == nil || *readings[0].ioc != want {
			t.Errorf("temperature %q = %#v, %v; want %v", raw, readings, err, want)
		}
	}
}

func TestParseStorCLITemperatureProbes(t *testing.T) {
	tests := []struct {
		name       string
		properties string
		wantIOC    *float64
		wantBoard  *float64
	}{
		{
			name:       "ROC only",
			properties: `{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}`,
			wantIOC:    float64Pointer(49),
		},
		{
			name: "ROC and Ctrl",
			properties: `{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"},` +
				`{"Ctrl_Prop":"Ctrl temperature(Degree Celsius)","Value":"45"}`,
			wantIOC:   float64Pointer(49),
			wantBoard: float64Pointer(45),
		},
		{
			name:       "controller temperature only",
			properties: `{"Ctrl_Prop":"Controller temperature(Degree Celsius)","Value":"45"}`,
			wantBoard:  float64Pointer(45),
		},
		{name: "no supported probe", properties: `{"Ctrl_Prop":"Ambient temperature","Value":"25"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[` + test.properties + `]}}]}`)
			readings, err := parseStorCLI(data)
			if err != nil {
				t.Fatal(err)
			}
			got := readings[0]
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

func TestRunStorCLI(t *testing.T) {
	installTestStorCLI(t, `printf 'normal output'`)
	output, err := runStorCLI(context.Background(), "discovery", "show")
	if err != nil || string(output) != "normal output" {
		t.Fatalf("output = %q, error = %v", output, err)
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

func TestParseStorCLIRejectsUnexpectedOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{name: "malformed JSON", data: `{`, want: "parse storcli JSON"},
		{name: "failed status", data: `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`, want: `status is "Failure"`},
		{name: "not a number", data: storCLIResponseWithTemperature("broken"), want: `invalid temperature "broken"`},
		{name: "NaN", data: storCLIResponseWithTemperature("NaN"), want: `invalid temperature "NaN"`},
		{name: "infinity", data: storCLIResponseWithTemperature("Inf"), want: `invalid temperature "Inf"`},
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
