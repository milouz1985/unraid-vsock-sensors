package sensors

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestStreamFramesSuccessiveSnapshots(t *testing.T) {
	var stream bytes.Buffer
	want := []Response{
		{Version: "one", Disks: []Disk{{ID: "disk1", Temp: 35}}},
		{Version: "two", HBAs: []HBA{{ID: "sas:1234", Temp: 51}}},
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
