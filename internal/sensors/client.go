package sensors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

// Fetch retrieves one sensor snapshot from the VSOCK server at cid and port.
// The context controls connection establishment and all subsequent I/O.
func Fetch(ctx context.Context, cid, port uint32) (Response, error) {
	var response Response
	conn, err := dialContext(ctx, cid, port)
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
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&response); err != nil {
		return response, fmt.Errorf("decode Unraid response: %w", err)
	}
	return response, nil
}

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
