// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"github.com/mdlayher/vsock"
	"io"
	"net"
	"sync"
	"testing"
	"time"
	"unraid-vsock-sensors/internal/sensors"
)

type snapshotPeerConn struct {
	net.Conn
	cid uint32
}

func (c *snapshotPeerConn) RemoteAddr() net.Addr {
	return &vsock.Addr{ContextID: c.cid, Port: defaultPort}
}

type snapshotTestListener struct {
	connections chan net.Conn
	accepted    chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
}

func newSnapshotTestListener(connections ...net.Conn) *snapshotTestListener {
	listener := &snapshotTestListener{
		connections: make(chan net.Conn, len(connections)),
		accepted:    make(chan struct{}, len(connections)),
		closed:      make(chan struct{}),
	}
	for _, conn := range connections {
		listener.connections <- conn
	}
	return listener
}

func (l *snapshotTestListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		l.accepted <- struct{}{}
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *snapshotTestListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *snapshotTestListener) Addr() net.Addr {
	return &vsock.Addr{ContextID: vsock.Local, Port: defaultPort}
}

func waitForSnapshotAccept(t *testing.T, listener *snapshotTestListener) {
	t.Helper()
	select {
	case <-listener.accepted:
	case <-time.After(time.Second):
		t.Fatal("snapshot connection was not accepted")
	}
}

func assertSnapshotConnectionClosed(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("connection read error = %v, want a closed connection", err)
		}
	case <-time.After(timeout):
		t.Fatal("connection remained open")
	}
}

func waitForSnapshotReceiver(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot receiver did not stop")
	}
}

func TestReceiveSnapshotsRejectsUnexpectedCID(t *testing.T) {
	const expectedCID = 3
	wrongServer, wrongClient := net.Pipe()
	validServer, validClient := net.Pipe()
	listener := newSnapshotTestListener(
		&snapshotPeerConn{Conn: wrongServer, cid: expectedCID + 1},
		&snapshotPeerConn{Conn: validServer, cid: expectedCID},
	)
	defer listener.Close()
	defer wrongClient.Close()
	defer validClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan receivedSnapshot)
	done := make(chan error, 1)
	go func() { done <- receiveSnapshots(ctx, listener, expectedCID, out) }()

	waitForSnapshotAccept(t, listener)
	assertSnapshotConnectionClosed(t, wrongClient, time.Second)
	waitForSnapshotAccept(t, listener)
	want := sensors.Response{
		Protocol: sensors.ProtocolVersion,
		Disks:    []sensors.Disk{}, HBAs: []sensors.HBA{},
	}
	if err := sensors.WriteFrame(validClient, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-out:
		if got.response.Protocol != want.Protocol {
			t.Fatalf("received protocol = %d, want %d", got.response.Protocol, want.Protocol)
		}
	case <-time.After(time.Second):
		t.Fatal("valid connection did not publish its snapshot")
	}

	cancel()
	_ = validClient.Close()
	waitForSnapshotReceiver(t, done)
}

func TestReceiveSnapshotsClosesSilentStreamAndAcceptsReconnect(t *testing.T) {
	const expectedCID = 3
	firstServer, firstClient := net.Pipe()
	secondServer, secondClient := net.Pipe()
	listener := newSnapshotTestListener(
		&snapshotPeerConn{Conn: firstServer, cid: expectedCID},
		&snapshotPeerConn{Conn: secondServer, cid: expectedCID},
	)
	defer listener.Close()
	defer firstClient.Close()
	defer secondClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan receivedSnapshot)
	done := make(chan error, 1)
	go func() { done <- receiveSnapshots(ctx, listener, expectedCID, out) }()

	waitForSnapshotAccept(t, listener)
	acceptedAt := time.Now()
	assertSnapshotConnectionClosed(t, firstClient, snapshotStreamTimeout+time.Second)
	if elapsed := time.Since(acceptedAt); elapsed < snapshotStreamTimeout-100*time.Millisecond {
		t.Fatalf("silent connection closed after %v, want at least %v", elapsed, snapshotStreamTimeout)
	}

	waitForSnapshotAccept(t, listener)
	want := sensors.Response{
		Protocol: sensors.ProtocolVersion,
		Disks:    []sensors.Disk{}, HBAs: []sensors.HBA{},
	}
	if err := sensors.WriteFrame(secondClient, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-out:
		if got.response.Protocol != want.Protocol {
			t.Fatalf("received protocol = %d, want %d", got.response.Protocol, want.Protocol)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected publisher did not publish its snapshot")
	}

	cancel()
	_ = secondClient.Close()
	waitForSnapshotReceiver(t, done)
}

func TestSnapshotQueueKeepsOnlyFreshestReading(t *testing.T) {
	ctx := context.Background()
	out := make(chan receivedSnapshot, 1)
	now := time.Now()
	first := receivedSnapshot{
		response: sensors.Response{Error: "first"}, receivedAt: now,
	}
	latest := receivedSnapshot{
		response: sensors.Response{Error: "latest"}, receivedAt: now.Add(time.Second),
	}
	if !sendLatestSnapshot(ctx, out, first) || !sendLatestSnapshot(ctx, out, latest) {
		t.Fatal("snapshot queue unexpectedly stopped")
	}
	if got := <-out; got.response.Error != latest.response.Error {
		t.Fatalf("queued snapshot = %q, want %q", got.response.Error, latest.response.Error)
	}
	if latest.expired(latest.receivedAt.Add(snapshotStreamTimeout - time.Nanosecond)) {
		t.Fatal("fresh snapshot was reported as expired")
	}
	if !latest.expired(latest.receivedAt.Add(snapshotStreamTimeout)) {
		t.Fatal("snapshot was accepted at its expiration deadline")
	}
}

// TestReceiveSnapshotsRecoversAfterInvalidFrame locks the contract that an
// invalid frame from an authorized peer (wrong CID is a different case) does
// not terminate receiveSnapshots: the connection is closed, the accept loop
// continues, and a subsequent valid connection is accepted and its snapshot
// published. It detects a global `return err` in the read-error path that
// would stop the whole receiver instead of just closing the connection.
func TestReceiveSnapshotsRecoversAfterInvalidFrame(t *testing.T) {
	const expectedCID = 3

	for _, test := range []struct {
		name string
		// sendInvalid writes an invalid frame to the first (bad) connection.
		sendInvalid func(client net.Conn) error
	}{
		{
			name: "invalid JSON",
			sendInvalid: func(client net.Conn) error {
				// A complete line that is not valid JSON.
				_, err := client.Write([]byte(`{"protocol": "not-a-number"}
`))
				return err
			},
		},
		{
			name: "unsupported protocol",
			sendInvalid: func(client net.Conn) error {
				return sensors.WriteFrame(client, sensors.Response{
					Protocol: sensors.ProtocolVersion + 1,
					Disks:    []sensors.Disk{},
					HBAs:     []sensors.HBA{},
				})
			},
		},
		{
			name: "oversized frame",
			sendInvalid: func(client net.Conn) error {
				// Write a line longer than maxFrameSize (1 MiB). The scanner
				// will reject it as "token too long". The write may fail with
				// a closed pipe if the receiver closes the connection mid-write;
				// that is acceptable because the frame is already being
				// rejected.
				payload := make([]byte, 1<<20+1024)
				for i := range payload {
					payload[i] = 'a'
				}
				payload[len(payload)-1] = '\n'
				_, err := client.Write(payload)
				if err != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)) {
					return nil
				}
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			badServer, badClient := net.Pipe()
			goodServer, goodClient := net.Pipe()
			listener := newSnapshotTestListener(
				&snapshotPeerConn{Conn: badServer, cid: expectedCID},
				&snapshotPeerConn{Conn: goodServer, cid: expectedCID},
			)
			defer listener.Close()
			defer badClient.Close()
			defer goodClient.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(chan receivedSnapshot)
			done := make(chan error, 1)
			go func() { done <- receiveSnapshots(ctx, listener, expectedCID, out) }()

			// The first (bad) connection is accepted.
			waitForSnapshotAccept(t, listener)

			// Send the invalid frame.
			if err := test.sendInvalid(badClient); err != nil {
				t.Fatalf("sending invalid frame: %v", err)
			}

			// The bad connection must be closed by the receiver.
			assertSnapshotConnectionClosed(t, badClient, time.Second)

			// The receiver must continue accepting: the second (good)
			// connection is accepted.
			waitForSnapshotAccept(t, listener)

			// The good connection sends a valid frame.
			want := sensors.Response{
				Protocol: sensors.ProtocolVersion,
				Disks:    []sensors.Disk{},
				HBAs:     []sensors.HBA{},
			}
			if err := sensors.WriteFrame(goodClient, want); err != nil {
				t.Fatal(err)
			}

			// The snapshot must be published.
			select {
			case got := <-out:
				if got.response.Protocol != want.Protocol {
					t.Fatalf("received protocol = %d, want %d", got.response.Protocol, want.Protocol)
				}
			case <-time.After(time.Second):
				t.Fatal("valid connection after invalid frame did not publish its snapshot")
			}

			cancel()
			_ = goodClient.Close()
			waitForSnapshotReceiver(t, done)
		})
	}
}
