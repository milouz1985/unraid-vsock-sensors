package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
)

type diskReading struct {
	Name       string  `json:"name"`
	Device     string  `json:"device"`
	Transport  string  `json:"transport"`
	Rotational bool    `json:"rotational"`
	Temp       float64 `json:"temp_c"`
}

type hbaReading struct {
	Name string  `json:"name"`
	Temp float64 `json:"temp_c"`
}

type unraidResponse struct {
	Disks     []diskReading `json:"disks"`
	HBAs      []hbaReading  `json:"hbas"`
	HBAError  string        `json:"hba_error"`
	DiskError string        `json:"error"`
}

func fetchUnraid(cid, port uint32) (unraidResponse, error) {
	var result unraidResponse
	conn, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return result, fmt.Errorf("connect to vsock %d:%d: %w", cid, port, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "GET\n"); err != nil {
		return result, err
	}
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("decode Unraid response: %w", err)
	}
	return result, nil
}

func sensorID(prefix, name string) string {
	var id strings.Builder
	id.WriteString(prefix)
	id.WriteByte('-')
	for _, char := range strings.ToLower(name) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			id.WriteRune(char)
		} else {
			id.WriteByte('-')
		}
	}
	return id.String()
}

func isNVMe(d diskReading) bool {
	return strings.EqualFold(d.Transport, "nvme") || strings.HasPrefix(d.Device, "nvme")
}

func isSSD(d diskReading) bool {
	return !d.Rotational && !isNVMe(d)
}
