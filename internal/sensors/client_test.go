package sensors

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSnapshotQueryProtocol(t *testing.T) {
	server, client := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer server.Close()
		err := json.NewEncoder(server).Encode(Response{
			Version: "test-version",
			Disks:   []Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
		})
		serverDone <- err
	}()

	response, err := fetchResponse(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if response.Version != "test-version" || len(response.Disks) != 1 || response.Disks[0].Temp != 35 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestSnapshotQueryLimitsResponseSize(t *testing.T) {
	server, client := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		_, _ = server.Write(append(bytes.Repeat([]byte(" "), maxResponseSize), []byte(`{}`)...))
	}()

	_, err := fetchResponse(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response should fail decoding, got %v", err)
	}
	<-serverDone
}

func TestSnapshotQueryRejectsTrailingData(t *testing.T) {
	server, client := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		_, _ = io.WriteString(server, `{"version":"test"} {}`)
	}()

	_, err := fetchResponse(context.Background(), client)
	if err == nil {
		t.Fatalf("trailing response data should fail decoding, got %v", err)
	}
	<-serverDone
}

func TestSnapshotQueryStopsWaitingWhenContextEnds(t *testing.T) {
	for _, test := range []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		cancel     bool
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			cancel: true,
		},
		{
			name: "deadline",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()

			ctx, cancel := test.newContext()
			defer cancel()
			if test.cancel {
				cancel()
			}
			if _, err := fetchResponse(ctx, client); err == nil {
				t.Fatal("fetch unexpectedly succeeded")
			}
		})
	}
}
