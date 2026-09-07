package sensors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

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
	if err := json.Unmarshal(r.scanner.Bytes(), &response); err != nil {
		return response, fmt.Errorf("decode stream snapshot: %w", err)
	}
	return response, nil
}
