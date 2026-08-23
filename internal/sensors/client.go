package sensors

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mdlayher/vsock"
)

func Fetch(cid, port uint32, timeout time.Duration) (Response, error) {
	var response Response
	conn, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return response, fmt.Errorf("connect to vsock %d:%d: %w", cid, port, err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, "GET\n"); err != nil {
		return response, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&response); err != nil {
		return response, fmt.Errorf("decode Unraid response: %w", err)
	}
	return response, nil
}
