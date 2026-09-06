package sensors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

type connection interface {
	io.ReadCloser
	SetDeadline(time.Time) error
}

const maxResponseSize = 1 << 20

// FetchUnix retrieves the latest snapshot exposed by the host-side hwmon
// daemon. Keeping this local query separate from the VSOCK stream lets the
// guest-to-host connection remain strictly one-way.
func FetchUnix(ctx context.Context, path string) (Response, error) {
	var response Response
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return response, fmt.Errorf("connect to %s: %w", path, err)
	}
	return fetchResponse(ctx, conn)
}

func fetchResponse(ctx context.Context, conn connection) (Response, error) {
	var response Response
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return response, fmt.Errorf("set snapshot query deadline: %w", err)
		}
	}
	// Moving the deadline to now interrupts a pending read or write on cancel.
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()

	data, err := io.ReadAll(io.LimitReader(conn, maxResponseSize+1))
	if err != nil {
		return response, fmt.Errorf("read snapshot response: %w", err)
	}
	if len(data) > maxResponseSize {
		return response, fmt.Errorf("snapshot response exceeds %d bytes", maxResponseSize)
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return response, fmt.Errorf("decode snapshot response: %w", err)
	}
	return response, nil
}

// DialVSOCK uses mdlayher/socket directly because mdlayher/vsock.Dial does
// not accept a context. The receiver can use vsock.Listen, while the publisher
// needs socket.Conn.Connect so a missing host is bounded by its caller.
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
