package sensors

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxFrameSize = 1 << 20

// WriteFrame writes one length-prefixed sensor snapshot. The fixed-size
// prefix lets a long-lived VSOCK connection carry successive JSON objects
// without relying on connection closure as framing.
func WriteFrame(out io.Writer, response Response) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode sensor snapshot: %w", err)
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("sensor snapshot exceeds %d bytes", maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(out, header[:]); err != nil {
		return fmt.Errorf("write sensor snapshot header: %w", err)
	}
	if err := writeAll(out, payload); err != nil {
		return fmt.Errorf("write sensor snapshot: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed sensor snapshot from a stream.
func ReadFrame(in io.Reader) (Response, error) {
	var response Response
	var header [4]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return response, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return response, fmt.Errorf("invalid sensor snapshot size %d", size)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(in, payload); err != nil {
		return response, fmt.Errorf("read sensor snapshot: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("decode sensor snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("decode sensor snapshot: unexpected trailing data")
	}
	return response, nil
}

func writeAll(out io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := out.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
