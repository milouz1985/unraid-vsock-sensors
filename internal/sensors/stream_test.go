package sensors

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestStreamFramesSuccessiveSnapshots(t *testing.T) {
	var stream bytes.Buffer
	want := []Response{
		{
			Protocol:    ProtocolVersion,
			Disks:       []Disk{{ID: "disk1", Name: "disk1", Device: "sda", Temp: 35}},
			HBADisabled: true,
		},
		{
			Protocol: ProtocolVersion, Disks: []Disk{},
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
		if !reflect.DeepEqual(got, want[index]) {
			t.Fatalf("frame %d = %#v, want %#v", index, got, want[index])
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("end of stream error = %v, want EOF", err)
	}
}

func TestStreamRejectsIncompatibleProtocol(t *testing.T) {
	for _, protocol := range []int{0, ProtocolVersion + 1} {
		frame := fmt.Sprintf(`{"protocol":%d,"disks":[],"hba_disabled":true}`+"\n", protocol)
		if _, err := NewFrameReader(strings.NewReader(frame)).Read(); err == nil {
			t.Fatalf("protocol %d was accepted", protocol)
		}
	}
}

func TestStreamRejectsOversizedMessage(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(strings.Repeat("x", maxFrameSize+1) + "\n"))
	if _, err := reader.Read(); err == nil {
		t.Fatal("oversized stream message was accepted")
	}
}
