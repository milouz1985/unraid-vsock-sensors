package sensors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

const maxFrameSize = 1 << 20

// DialVSOCK connects the publisher to the host without leaving connection
// establishment outside its context deadline.
func DialVSOCK(ctx context.Context, cid, port uint32) (*socket.Conn, error) {
	conn, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Connect(ctx, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// WriteFrame writes one newline-delimited JSON snapshot to a persistent stream.
func WriteFrame(out io.Writer, response Response) error {
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return fmt.Errorf("encode stream snapshot: %w", err)
	}
	return nil
}

// FrameReader reads successive newline-delimited JSON snapshots while limiting
// each one to maxFrameSize bytes.
type FrameReader struct {
	scanner *bufio.Scanner
}

func NewFrameReader(in io.Reader) *FrameReader {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), maxFrameSize+1)
	return &FrameReader{scanner: scanner}
}

func (r *FrameReader) Read() (Response, error) {
	var response Response
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return response, fmt.Errorf("read stream snapshot: %w", err)
		}
		return response, io.EOF
	}
	response, err := decodeSnapshot(r.scanner.Bytes())
	if err != nil {
		return response, fmt.Errorf("decode stream snapshot: %w", err)
	}
	return response, nil
}

type snapshotFrame struct {
	Version     *string         `json:"version"`
	Timestamp   *time.Time      `json:"timestamp"`
	Disks       json.RawMessage `json:"disks"`
	HBAs        json.RawMessage `json:"hbas"`
	HBADisabled bool            `json:"hba_disabled,omitempty"`
	HBAError    string          `json:"hba_error,omitempty"`
	Error       string          `json:"error,omitempty"`
}

type diskFrame struct {
	ID          *string  `json:"id"`
	Name        *string  `json:"name"`
	Device      *string  `json:"device"`
	Transport   string   `json:"transport,omitempty"`
	Rotational  bool     `json:"rotational"`
	Temp        *float64 `json:"temp_c"`
	Unavailable bool     `json:"unavailable,omitempty"`
}

type hbaFrame struct {
	ID         *string  `json:"id"`
	Model      string   `json:"model,omitempty"`
	PCIAddress string   `json:"pci_address,omitempty"`
	Temp       *float64 `json:"temp_c"`
}

func decodeSnapshot(data []byte) (Response, error) {
	var frame snapshotFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return Response{}, err
	}
	if frame.Version == nil || *frame.Version == "" {
		return Response{}, fmt.Errorf("missing version")
	}
	if frame.Timestamp == nil || frame.Timestamp.IsZero() {
		return Response{}, fmt.Errorf("missing timestamp")
	}
	if len(frame.Disks) == 0 {
		return Response{}, fmt.Errorf("missing disks inventory")
	}

	response := Response{
		Version: *frame.Version, Timestamp: *frame.Timestamp,
		HBADisabled: frame.HBADisabled, HBAError: frame.HBAError, Error: frame.Error,
	}
	if !isJSONNull(frame.Disks) {
		var disks []diskFrame
		if err := json.Unmarshal(frame.Disks, &disks); err != nil {
			return Response{}, fmt.Errorf("decode disks inventory: %w", err)
		}
		response.Disks = make([]Disk, 0, len(disks))
		for index, disk := range disks {
			if disk.ID == nil || *disk.ID == "" || disk.Name == nil || *disk.Name == "" ||
				disk.Device == nil || *disk.Device == "" || disk.Temp == nil {
				return Response{}, fmt.Errorf("incomplete disk at index %d", index)
			}
			response.Disks = append(response.Disks, Disk{
				ID: *disk.ID, Name: *disk.Name, Device: *disk.Device,
				Transport: disk.Transport, Rotational: disk.Rotational,
				Temp: *disk.Temp, Unavailable: disk.Unavailable,
			})
		}
	} else if frame.Error == "" {
		return Response{}, fmt.Errorf("null disks inventory without collection error")
	}

	if len(frame.HBAs) != 0 && !isJSONNull(frame.HBAs) {
		var hbas []hbaFrame
		if err := json.Unmarshal(frame.HBAs, &hbas); err != nil {
			return Response{}, fmt.Errorf("decode HBA inventory: %w", err)
		}
		response.HBAs = make([]HBA, 0, len(hbas))
		for index, hba := range hbas {
			if hba.ID == nil || *hba.ID == "" || hba.Temp == nil {
				return Response{}, fmt.Errorf("incomplete HBA at index %d", index)
			}
			response.HBAs = append(response.HBAs, HBA{
				ID: *hba.ID, Model: hba.Model, PCIAddress: hba.PCIAddress, Temp: *hba.Temp,
			})
		}
	}
	if frame.HBADisabled && len(response.HBAs) != 0 {
		return Response{}, fmt.Errorf("disabled HBA collection contains sensors")
	}
	if !frame.HBADisabled && frame.HBAError == "" && len(response.HBAs) == 0 {
		return Response{}, fmt.Errorf("empty HBA inventory without collection error")
	}
	return response, nil
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
