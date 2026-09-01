package sensors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

type connection interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

type dialer func(context.Context, uint32, uint32) (connection, error)

const maxResponseSize = 1 << 20

// Fetch retrieves one sensor snapshot from the VSOCK server at cid and port.
// The context controls connection establishment and all subsequent I/O.
func Fetch(ctx context.Context, cid, port uint32) (Response, error) {
	return fetchWithDialer(ctx, cid, port, func(ctx context.Context, cid, port uint32) (connection, error) {
		return dialContext(ctx, cid, port)
	})
}

func fetchWithDialer(ctx context.Context, cid, port uint32, dial dialer) (Response, error) {
	var response Response
	conn, err := dial(ctx, cid, port)
	if err != nil {
		return response, fmt.Errorf("connect to vsock %d:%d: %w", cid, port, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return response, fmt.Errorf("set vsock deadline: %w", err)
		}
	}
	// Moving the deadline to now interrupts a pending read or write on cancel.
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()

	if _, err := io.WriteString(conn, "GET\n"); err != nil {
		return response, fmt.Errorf("write Unraid request: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(conn, maxResponseSize+1))
	if err != nil {
		return response, fmt.Errorf("read Unraid response: %w", err)
	}
	if len(data) > maxResponseSize {
		return response, fmt.Errorf("Unraid response exceeds %d bytes", maxResponseSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("decode Unraid response: %w", err)
	}
	// A second decode must reach EOF: the protocol permits exactly one JSON
	// object, optionally followed by whitespace, and no trailing value or data.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Response{}, errors.New("decode Unraid response: unexpected trailing data")
	}
	return response, nil
}

// dialContext uses mdlayher/socket directly because mdlayher/vsock.Dial does
// not accept a context. The server can use vsock.Listen, while the client needs
// socket.Conn.Connect so a missing or unreachable VM is bounded by its caller.
func dialContext(ctx context.Context, cid, port uint32) (*socket.Conn, error) {
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
