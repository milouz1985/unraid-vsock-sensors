package sensors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const maxFrameSize = 1 << 20

// WriteFrame writes one newline-delimited JSON message to a persistent stream.
func WriteFrame(out io.Writer, message Message) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode stream message: %w", err)
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("stream message exceeds %d bytes", maxFrameSize)
	}
	payload = append(payload, '\n')
	if _, err := io.Copy(out, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write stream message: %w", err)
	}
	return nil
}

// FrameReader reads successive newline-delimited JSON messages while limiting
// each one to maxFrameSize bytes.
type FrameReader struct {
	scanner *bufio.Scanner
}

func NewFrameReader(in io.Reader) *FrameReader {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), maxFrameSize+1)
	return &FrameReader{scanner: scanner}
}

func (r *FrameReader) Read() (Message, error) {
	var message Message
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return message, fmt.Errorf("read stream message: %w", err)
		}
		return message, io.EOF
	}
	if err := json.Unmarshal(r.scanner.Bytes(), &message); err != nil {
		return message, fmt.Errorf("decode stream message: %w", err)
	}
	return message, nil
}
