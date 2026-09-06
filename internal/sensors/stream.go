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

// WriteFrame writes one length-prefixed stream message. The fixed-size
// prefix lets a long-lived VSOCK connection carry successive JSON objects
// without relying on connection closure as framing.
func WriteFrame(out io.Writer, message Message) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode stream message: %w", err)
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("stream message exceeds %d bytes", maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(out, header[:]); err != nil {
		return fmt.Errorf("write stream message header: %w", err)
	}
	if err := writeAll(out, payload); err != nil {
		return fmt.Errorf("write stream message: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed message from a stream.
func ReadFrame(in io.Reader) (Message, error) {
	var message Message
	var header [4]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return message, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return message, fmt.Errorf("invalid stream message size %d", size)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(in, payload); err != nil {
		return message, fmt.Errorf("read stream message: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&message); err != nil {
		return message, fmt.Errorf("decode stream message: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Message{}, errors.New("decode stream message: unexpected trailing data")
	}
	return message, nil
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
