// SPDX-License-Identifier: GPL-3.0-or-later

package sensors

import (
	"bytes"
	"errors"
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
			Protocol: ProtocolVersion,
			Disks:    []Disk{{ID: "disk1", Name: "disk1", Device: "sda", Temp: 35}},
			HBAs:     []HBA{},
		},
		{
			Protocol: ProtocolVersion, Disks: []Disk{},
			HBAs: []HBA{{ID: "sas:1234", Temp: 51}},
		},
		{
			Protocol: ProtocolVersion, Disks: []Disk{},
			HBAs: []HBA{{
				ID: "sas:5678", Temp: 52,
				IOCTemp: float64Pointer(52), BoardTemp: float64Pointer(48),
			}},
		},
	}
	for _, response := range want {
		if err := WriteFrame(&stream, response); err != nil {
			t.Fatal(err)
		}
	}
	// Pin the wire format independently of the writer/reader roundtrip.
	const wire = `{"protocol":1,"disks":[{"id":"disk1","name":"disk1","device":"sda","rotational":false,"temp_c":35}],"hbas":[]}
{"protocol":1,"disks":[],"hbas":[{"id":"sas:1234","temp_c":51}]}
{"protocol":1,"disks":[],"hbas":[{"id":"sas:5678","temp_c":52,"ioc_temp_c":52,"board_temp_c":48}]}
`
	if got := stream.String(); got != wire {
		t.Fatalf("wire frames = %q, want %q", got, wire)
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

func float64Pointer(value float64) *float64 {
	return &value
}

func TestStreamRejectsIncompatibleProtocol(t *testing.T) {
	for _, protocol := range []int{0, ProtocolVersion + 1} {
		frame := fmt.Sprintf(`{"protocol":%d,"disks":[],"hbas":[]}`+"\n", protocol)
		if _, err := NewFrameReader(strings.NewReader(frame)).Read(); err == nil {
			t.Fatalf("protocol %d was accepted", protocol)
		}
	}
}

func TestStreamRejectsOversizedMessage(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(strings.Repeat("x", maxFrameSize+1) + "\n"))
	if _, err := reader.Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized stream error = %v, want ErrFrameTooLarge", err)
	}
}

func TestStreamFrameSizeBoundaries(t *testing.T) {
	// This literal pins the encoded overhead independently of WriteFrame.
	const emptyName = `{"protocol":1,"disks":[{"id":"disk1","name":"","device":"sda","rotational":false,"temp_c":35}],"hbas":[]}` + "\n"
	const limit = 1 << 20 // Wire contract: 1 MiB, including LF.
	for _, size := range []int{256, limit - 1, limit, limit + 1} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			name := strings.Repeat("a", size-len(emptyName))
			wire := strings.Replace(emptyName, `"name":""`, `"name":"`+name+`"`, 1)
			response := Response{
				Protocol: 1,
				Disks:    []Disk{{ID: "disk1", Name: name, Device: "sda", Temp: 35}},
				HBAs:     []HBA{},
			}
			if len(wire) != size {
				t.Fatalf("fixture size = %d, want %d", len(wire), size)
			}
			var out streamRecordingWriter
			writeErr := WriteFrame(&out, response)
			got, readErr := NewFrameReader(strings.NewReader(wire)).Read()
			if size > limit {
				if !errors.Is(writeErr, ErrFrameTooLarge) || !errors.Is(readErr, ErrFrameTooLarge) {
					t.Fatalf("oversized errors: write=%v, read=%v", writeErr, readErr)
				}
				if out.calls != 0 || out.Len() != 0 {
					t.Fatalf("oversized frame wrote %d bytes in %d calls", out.Len(), out.calls)
				}
				return
			}
			if writeErr != nil || readErr != nil {
				t.Fatalf("valid frame errors: write=%v, read=%v", writeErr, readErr)
			}
			if out.calls != 1 || out.String() != wire {
				t.Fatalf("wire mismatch: %d bytes in %d calls", out.Len(), out.calls)
			}
			if !reflect.DeepEqual(got, response) {
				t.Fatal("reader changed valid boundary frame")
			}
		})
	}
}

func TestWriteFrameLimitsEncodedSize(t *testing.T) {
	// Each '<' occupies six wire bytes (\u003c). Raw text is below 1 MiB,
	// while the encoded snapshot is above it.
	response := Response{Protocol: 1, HBAs: []HBA{{ID: strings.Repeat("<", 180_000)}}}
	var out streamRecordingWriter
	if err := WriteFrame(&out, response); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("escaped oversized frame error = %v, want ErrFrameTooLarge", err)
	}
	if out.calls != 0 {
		t.Fatalf("escaped oversized frame made %d writes", out.calls)
	}
}

func TestWriteFramePropagatesWriteFailure(t *testing.T) {
	want := errors.New("broken stream")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "write error", err: want, want: want},
		{name: "short write", want: io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := streamFailingWriter{err: test.err}
			if err := WriteFrame(out, Response{Protocol: 1}); !errors.Is(err, test.want) {
				t.Fatalf("write error = %v, want %v", err, test.want)
			}
		})
	}
}

type streamRecordingWriter struct {
	bytes.Buffer
	calls int
}

func (w *streamRecordingWriter) Write(data []byte) (int, error) {
	w.calls++
	return w.Buffer.Write(data)
}

type streamFailingWriter struct{ err error }

func (w streamFailingWriter) Write([]byte) (int, error) { return 0, w.err }
