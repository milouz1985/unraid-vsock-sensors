package sensors

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamFramesSuccessiveSnapshots(t *testing.T) {
	var stream bytes.Buffer
	now := time.Now().UTC()
	want := []Response{
		{
			Version: "one", Timestamp: now,
			Disks:       []Disk{{ID: "disk1", Name: "disk1", Device: "sda", Temp: 35}},
			HBADisabled: true,
		},
		{
			Version: "two", Timestamp: now.Add(time.Second), Disks: []Disk{},
			HBAs: []HBA{{ID: "sas:1234", Temp: 51}},
		},
	}
	for _, response := range want {
		if err := WriteFrame(&stream, response); err != nil {
			t.Fatal(err)
		}
	}
	reader := NewFrameReader(&stream)
	for index := range want {
		got, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != want[index].Version {
			t.Fatalf("frame %d version = %q, want %q", index, got.Version, want[index].Version)
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("end of stream error = %v, want EOF", err)
	}
}

func TestStreamRejectsIncompleteSnapshots(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for name, frame := range map[string]string{
		"null snapshot":       `null`,
		"empty object":        `{}`,
		"missing disks":       `{"version":"1","timestamp":"` + now + `","hba_disabled":true}`,
		"null disks":          `{"version":"1","timestamp":"` + now + `","disks":null,"hba_disabled":true}`,
		"missing disk temp":   `{"version":"1","timestamp":"` + now + `","disks":[{"id":"1","name":"disk1","device":"sda"}],"hba_disabled":true}`,
		"null disk temp":      `{"version":"1","timestamp":"` + now + `","disks":[{"id":"1","name":"disk1","device":"sda","temp_c":null}],"hba_disabled":true}`,
		"missing HBA temp":    `{"version":"1","timestamp":"` + now + `","disks":[],"hbas":[{"id":"sas:1"}]}`,
		"empty enabled HBAs":  `{"version":"1","timestamp":"` + now + `","disks":[]}`,
		"disabled HBA sensor": `{"version":"1","timestamp":"` + now + `","disks":[],"hbas":[{"id":"sas:1","temp_c":40}],"hba_disabled":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFrameReader(strings.NewReader(frame + "\n")).Read(); err == nil {
				t.Fatal("incomplete snapshot was accepted")
			}
		})
	}
}

func TestStreamAcceptsExplicitEmptyAndZeroValues(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	frames := []string{
		`{"version":"1","timestamp":"` + now + `","disks":[],"hba_disabled":true}`,
		`{"version":"1","timestamp":"` + now + `","disks":[{"id":"1","name":"disk1","device":"sda","temp_c":0}],"hba_error":"unavailable"}`,
		`{"version":"1","timestamp":"` + now + `","disks":null,"error":"unavailable","hba_disabled":true}`,
	}
	for _, frame := range frames {
		if _, err := NewFrameReader(strings.NewReader(frame + "\n")).Read(); err != nil {
			t.Fatalf("valid snapshot was rejected: %v", err)
		}
	}
}

func TestStreamRejectsOversizedMessage(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(strings.Repeat("x", maxFrameSize+1) + "\n"))
	if _, err := reader.Read(); err == nil {
		t.Fatal("oversized stream message was accepted")
	}
}
