// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

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

func TestParseStorCLI(t *testing.T) {
	for _, spelling := range []string{"Celsius", "Celcius"} {
		t.Run(spelling, func(t *testing.T) {
			property := "ROC temperature(Degree " + spelling + ")"
			data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"` + property + `","Value":"49"}]}}]}`)
			readings, err := parseStorCLI(data)
			if err != nil || len(readings) != 1 || readings[0] != 49 {
				t.Fatalf("got %#v, %v", readings, err)
			}
		})
	}
	for raw, want := range map[string]float64{"0": 0, "-40": -40, "151": 151, "255": 255} {
		readings, err := parseStorCLI([]byte(storCLIResponseWithTemperature(raw)))
		if err != nil || readings[0] != want {
			t.Errorf("temperature %q = %#v, %v; want %v", raw, readings, err, want)
		}
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
	if got, want := metadata[4], (hbaMetadata{id: "serial:SERIAL-4", model: "OEM HBA"}); got != want {
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
