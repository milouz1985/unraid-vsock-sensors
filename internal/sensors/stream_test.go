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

func TestStreamRejectsOversizedMessage(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(strings.Repeat("x", maxFrameSize+1) + "\n"))
	if _, err := reader.Read(); err == nil {
		t.Fatal("oversized stream message was accepted")
	}
}
