package sensors

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestStreamFramesSuccessiveMessages(t *testing.T) {
	var stream bytes.Buffer
	want := []Message{
		{Type: MessageDisks, Response: Response{Version: "one", Disks: []Disk{{ID: "disk1", Temp: 35}}}},
		{Type: MessageHBAs, Response: Response{Version: "two", HBAs: []HBA{{ID: "sas:1234", Temp: 51}}}},
	}
	for _, response := range want {
		if err := WriteFrame(&stream, response); err != nil {
			t.Fatal(err)
		}
	}
	for index := range want {
		got, err := ReadFrame(&stream)
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != want[index].Type || got.Version != want[index].Version {
			t.Fatalf("frame %d version = %q, want %q", index, got.Version, want[index].Version)
		}
	}
	if _, err := ReadFrame(&stream); err != io.EOF {
		t.Fatalf("end of stream error = %v, want EOF", err)
	}
}

func TestStreamRejectsInvalidFrameSize(t *testing.T) {
	for _, size := range []uint32{0, maxFrameSize + 1} {
		var stream bytes.Buffer
		if err := binary.Write(&stream, binary.BigEndian, size); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFrame(&stream); err == nil || !strings.Contains(err.Error(), "invalid stream message size") {
			t.Fatalf("size %d returned %v", size, err)
		}
	}
}

type shortWriter struct {
	bytes.Buffer
}

func (w *shortWriter) Write(data []byte) (int, error) {
	if len(data) > 1 {
		data = data[:1]
	}
	return w.Buffer.Write(data)
}

func TestWriteFrameHandlesShortWrites(t *testing.T) {
	var stream shortWriter
	if err := WriteFrame(&stream, Message{Type: MessageHeartbeat, Response: Response{Version: "test"}}); err != nil {
		t.Fatal(err)
	}
	response, err := ReadFrame(&stream.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != "test" {
		t.Fatalf("version = %q, want test", response.Version)
	}
}
