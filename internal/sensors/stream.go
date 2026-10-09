// SPDX-License-Identifier: GPL-3.0-or-later

package sensors

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxFrameSize includes the terminating newline.
const maxFrameSize = 1 << 20

// ErrFrameTooLarge identifies a snapshot exceeding the wire size limit.
var ErrFrameTooLarge = errors.New("stream snapshot exceeds size limit")

// WriteFrame writes one newline-delimited JSON snapshot to a persistent stream.
func WriteFrame(out io.Writer, response Response) error {
	data, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode stream snapshot: %w", err)
	}
	if len(data) >= maxFrameSize {
		return fmt.Errorf("%w: %d bytes including newline, maximum %d", ErrFrameTooLarge, len(data)+1, maxFrameSize)
	}
	data = append(data, '\n')
	n, err := out.Write(data)
	if err != nil {
		return fmt.Errorf("write stream snapshot: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write stream snapshot: %w", io.ErrShortWrite)
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
	scanner.Buffer(make([]byte, 4096), maxFrameSize)
	return &FrameReader{scanner: scanner}
}

func (r *FrameReader) Read() (Response, error) {
	var response Response
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				return response, fmt.Errorf("read stream snapshot: %w", ErrFrameTooLarge)
			}
			return response, fmt.Errorf("read stream snapshot: %w", err)
		}
		return response, io.EOF
	}
	if err := json.Unmarshal(r.scanner.Bytes(), &response); err != nil {
		return response, fmt.Errorf("decode stream snapshot: %w", err)
	}
	if response.Protocol != ProtocolVersion {
		return Response{}, fmt.Errorf(
			"unsupported snapshot protocol %d (expected %d)",
			response.Protocol, ProtocolVersion,
		)
	}
	return response, nil
}
