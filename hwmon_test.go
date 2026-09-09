package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"

	"github.com/mdlayher/vsock"
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

func TestMakeHWMonSamples(t *testing.T) {
	state := sensors.Response{
		Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
			{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
		},
		HBAs: []sensors.HBA{{ID: "sas:1234", Model: "SAS3008", PCIAddress: "0000:06:10.0", Temp: 51}},
	}

	disks, hbas := makeHWMonSamples(state)
	wantDisks := []hwmonSample{
		hwmonTestSample("disk:group:hdd", "HDD maximum", 38),
		hwmonTestSample("disk:1", "disk1 (sda)", 34),
		hwmonTestSample("disk:2", "disk2 (sdb)", 38),
		hwmonTestSample("disk:3", "cache (nvme0n1)", 45),
	}
	wantHBAs := []hwmonSample{hwmonTestSample("hba:sas:1234", "SAS3008 (0000:06:10.0)", 51)}
	if !reflect.DeepEqual(disks, wantDisks) {
		t.Fatalf("disk readings = %#v, want %#v", disks, wantDisks)
	}
	if !reflect.DeepEqual(hbas, wantHBAs) {
		t.Fatalf("HBA readings = %#v, want %#v", hbas, wantHBAs)
	}
}

func TestMakeHWMonSamplesFailsSafeUnavailableDiskAndItsGroup(t *testing.T) {
	state := sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}}
	want := []hwmonSample{
		hwmonTestSample("disk:group:hdd", "HDD maximum", hwmonFailsafeTemp),
		hwmonTestSample("disk:1", "disk1 (sda)", 35),
		hwmonTestSample("disk:2", "disk2 (sdb)", hwmonFailsafeTemp),
		hwmonTestSample("disk:3", "cache (nvme0n1)", 46),
	}
	if got := makeDiskSamples(state); !reflect.DeepEqual(got, want) {
		t.Fatalf("disk readings = %#v, want explicit disk and group failsafe %#v", got, want)
	}
}

func TestEncodeHWMonSamples(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("disk:1", "disk1 (sda)", 34.125),
		hwmonTestSample("disk:group:hdd", "HDD maximum", 38),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:1\t34125\tdisk1 (sda)\n" +
		"sample\tdisk:group:hdd\t38000\tHDD maximum\n" +
		"commit\tdisk\n"
	if got := output.String(); got != want {
		t.Fatalf("encoded snapshot = %q, want %q", got, want)
	}
}

func TestEncodeHWMonSamplesRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		reading   hwmonSample
	}{
		{name: "namespace", namespace: "other", reading: hwmonTestSample("other:1", "disk1", 30)},
		{name: "wrong prefix", namespace: "disk", reading: hwmonTestSample("hba:0", "hba0", 30)},
		{name: "newline", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1\nbad", 30)},
		{name: "NaN", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1", math.NaN())},
		{name: "huge", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1", 1e300)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := encodeHWMonSamples(&bytes.Buffer{}, test.namespace, "commit", []hwmonSample{test.reading}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEncodeHWMonSamplesValidatesBeforeWriting(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("disk:1", "disk1", 30),
		hwmonTestSample("disk:2", "invalid\nlabel", 31),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err == nil {
		t.Fatal("expected validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
	}
}

func TestEncodeHWMonSamplesRejectsDuplicateIDsBeforeWriting(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("hba:serial:1234", "hba0", 50),
		hwmonTestSample("hba:serial:1234", "hba1", 51),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "hba", "commit", readings); err == nil {
		t.Fatal("expected duplicate ID error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
	}
}

func TestPublishHWMonStateKeepsFamiliesIndependent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Error: "disks.ini failed",
		HBAs:  []sensors.HBA{{ID: "sas:1234", Temp: 51}},
	}
	publisher := &hwmonPublisher{cachePath: filepath.Join(directory, "inventory.json")}
	_, err := publisher.publish(path, state)
	if err == nil || err.Error() != "disks: disks.ini failed" {
		t.Fatalf("error = %v, want disks.ini failure only", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(data), "sample\thba:sas:1234\t51000\tsas:1234\nconfigure\thba\n"; got != want {
		t.Fatalf("HBA snapshot = %q, want %q", got, want)
	}
}

func TestPublisherDistinguishesMissingAndEmptyDiskInventory(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks: hwmonInventory{initialized: true, sensors: []hwmonSensor{
			{id: "disk:serial", label: "disk1 (sda)"},
		}},
		hbas: hwmonInventory{initialized: true},
	}

	_, err := publisher.publish(device, sensors.Response{})
	if err == nil || !strings.Contains(err.Error(), "disks: inventory is missing") {
		t.Fatalf("missing inventory error = %v", err)
	}
	if len(publisher.disks.sensors) != 1 {
		t.Fatal("missing inventory changed the configured disk")
	}

	changed, err := publisher.publish(device, sensors.Response{
		Disks: []sensors.Disk{}, HBAs: []sensors.HBA{},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.disks.sensors) != 0 {
		t.Fatalf("disk inventory = %#v, want empty", publisher.disks.sensors)
	}

	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: publisher.cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	if !restored.disks.initialized || len(restored.disks.sensors) != 0 {
		t.Fatalf("restored disk inventory = %#v, want initialized and empty", restored.disks)
	}
}

func TestPublisherRestoresCachedInventoryAtFailsafe(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: cache,
		disks: hwmonInventory{initialized: true, sensors: []hwmonSensor{
			{id: "disk:serial", label: "disk1 (sda)"},
		}},
	}
	if err := publisher.saveCache(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("cache mode = %04o, want 0600", mode)
	}
	legacy := `{"version":1,"disks":{"readings":[{"id":"disk:serial","label":"disk1 (sda)","members":["disk:serial"]}]}}`
	if err := os.WriteFile(cache, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: cache}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(device)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:serial\t100000\tdisk1 (sda)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("restored inventory = %q, want failsafe inventory %q", got, want)
	}
}

func TestPublisherReportsPartialCacheRestore(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"disks":{"readings":[{"id":"disk:serial","label":"disk1"}]},"hbas":{"readings":[{"id":"invalid","label":"HBA"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	err := (&hwmonPublisher{cachePath: cache}).restore(device)
	if err == nil || !strings.Contains(err.Error(), "restore HBA") {
		t.Fatalf("err=%v", err)
	}
	contents, readErr := os.ReadFile(device)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(contents), "sample\tdisk:serial\t100000\tdisk1\nconfigure\tdisk\n"; got != want {
		t.Fatalf("restored disk inventory = %q, want %q", got, want)
	}
}

func TestParseRestartUnits(t *testing.T) {
	units, err := parseRestartUnits("coolercontrold.service, fan2go.service,coolercontrold.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"coolercontrold.service", "fan2go.service"}; !reflect.DeepEqual(units, want) {
		t.Fatalf("units = %#v, want %#v", units, want)
	}
	if _, err := parseRestartUnits("--no-block"); err == nil {
		t.Fatal("option-like unit must be rejected")
	}
}

func makeDiskSamples(state sensors.Response) []hwmonSample {
	disks, _ := makeHWMonSamples(state)
	return disks
}

func hwmonTestSample(id, label string, temperature float64) hwmonSample {
	return hwmonSample{
		sensor: hwmonSensor{id: id, label: label}, temperature: temperature,
	}
}

func TestPublisherReportsReconfigurationWhenCacheSaveFails(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	blockingFile := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockingFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{cachePath: filepath.Join(blockingFile, "inventory.json")}
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34}},
		HBAs:  []sensors.HBA{},
	}
	reconfigured, err := publisher.publish(device, state)
	if !reconfigured {
		t.Fatal("kernel reconfiguration must be reported even when the cache cannot be saved")
	}
	if err == nil || !strings.Contains(err.Error(), "save hwmon inventory cache") {
		t.Fatalf("error = %v, want cache save failure", err)
	}
	if !publisher.cacheDirty {
		t.Fatal("failed cache save must remain pending for the next cycle")
	}
}
