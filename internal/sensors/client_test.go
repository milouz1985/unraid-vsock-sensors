package sensors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		request, err := bufio.NewReader(server).ReadString('\n')
		if err == nil && request != "GET\n" {
			err = fmt.Errorf("unexpected request %q", request)
		}
		if err == nil {
			err = json.NewEncoder(server).Encode(Response{
				Version: "test-version",
				Disks:   []Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
			})
		}
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
		_, _ = bufio.NewReader(server).ReadString('\n')
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
		_, _ = bufio.NewReader(server).ReadString('\n')
		_, _ = io.WriteString(server, `{"version":"test"} {}`)
	}()

	_, err := fetchResponse(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "unexpected trailing data") {
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
			requestRead := make(chan struct{})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				defer server.Close()
				_, _ = bufio.NewReader(server).ReadString('\n')
				close(requestRead)
				_, _ = bufio.NewReader(server).ReadByte()
			}()

			ctx, cancel := test.newContext()
			defer cancel()
			fetchDone := make(chan error, 1)
			go func() {
				_, err := fetchResponse(ctx, client)
				fetchDone <- err
			}()
			<-requestRead
			if test.cancel {
				cancel()
			}

			select {
			case err := <-fetchDone:
				if err == nil {
					t.Fatal("fetch unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				client.Close()
				t.Fatal("context did not interrupt the pending response read")
			}
			<-serverDone
		})
	}
}
