// SPDX-License-Identifier: GPL-3.0-or-later

package sensors

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const maxFrameSize = 1 << 20

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
