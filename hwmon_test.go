package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
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

func TestPublisherOmitsUnavailableDiskAndItsGroupFromCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	current := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, current); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:1\t35000\tdisk1 (sda)\n" +
		"sample\tdisk:3\t46000\tcache (nvme0n1)\n" +
		"commit\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("update = %q, want failed disk and group omitted %q", got, want)
	}
}

func TestPublisherConfiguresUnavailableDiskAndItsGroupAtFailsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	readings := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
	}})
	if _, err := publishHWMonFamily(path, "disk", &hwmonInventory{}, readings); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:group:hdd\t100000\tHDD maximum\n" +
		"sample\tdisk:1\t35000\tdisk1 (sda)\n" +
		"sample\tdisk:2\t100000\tdisk2 (sdb)\n" +
		"configure\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("configuration = %q, want failed disk and group at failsafe %q", got, want)
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

func TestEncodeHWMonSamplesAllowsEmptyCommitSubset(t *testing.T) {
	readings := []hwmonSample{{
		sensor:       hwmonSensor{id: "disk:1", label: "disk1 (sda)"},
		temperature:  hwmonFailsafeTemp,
		omitOnCommit: true,
	}}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "commit\tdisk\n"; got != want {
		t.Fatalf("encoded snapshot = %q, want empty subset %q", got, want)
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

func TestPublisherReconfiguresChangedTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{}
	states := [][]hwmonSample{
		makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
		}}),
		makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		}}),
	}
	for index, readings := range states {
		if index > 0 {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := publishHWMonFamily(path, "disk", &publisher.disks, readings); err != nil {
			t.Fatalf("state %d: %v", index, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:1\t35000\tdisk1 (sda)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("update = %q, want replacement inventory %q", got, want)
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

func TestPublisherDistinguishesMissingAndEmptyHBAInventory(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks:     hwmonInventory{initialized: true},
		hbas: hwmonInventory{initialized: true, sensors: []hwmonSensor{
			{id: "hba:sas:1234", label: "SAS3008 (0000:06:10.0)"},
		}},
	}

	_, err := publisher.publish(device, sensors.Response{Disks: []sensors.Disk{}})
	if err == nil || !strings.Contains(err.Error(), "HBA: inventory is missing") {
		t.Fatalf("missing inventory error = %v", err)
	}
	if len(publisher.hbas.sensors) != 1 {
		t.Fatal("missing inventory changed the configured HBA")
	}

	changed, err := publisher.publish(device, sensors.Response{
		Disks: []sensors.Disk{}, HBAs: []sensors.HBA{},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.hbas.sensors) != 0 {
		t.Fatalf("HBA inventory = %#v, want empty", publisher.hbas.sensors)
	}

	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: publisher.cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	if !restored.hbas.initialized || len(restored.hbas.sensors) != 0 {
		t.Fatalf("restored HBA inventory = %#v, want initialized and empty", restored.hbas)
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

func TestPublisherContinuesCacheRestoreAfterDiskFailure(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"disks":{"readings":[{"id":"invalid","label":"disk1"}]},"hbas":{"readings":[{"id":"hba:sas:1234","label":"SAS3008"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	err := (&hwmonPublisher{cachePath: cache}).restore(device)
	if err == nil || !strings.Contains(err.Error(), "restore disks") {
		t.Fatalf("err=%v", err)
	}
	contents, readErr := os.ReadFile(device)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(contents), "sample\thba:sas:1234\t100000\tSAS3008\nconfigure\thba\n"; got != want {
		t.Fatalf("restored HBA inventory = %q, want %q", got, want)
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
	for _, value := range []string{
		"--no-block",
		"coolercontrold*",
		"fan?go.service",
		"[cf]an.service",
	} {
		t.Run("reject "+value, func(t *testing.T) {
			if _, err := parseRestartUnits(value); err == nil {
				t.Fatalf("invalid unit %q accepted", value)
			}
		})
	}
	for _, value := range []string{
		"unraid-vsock-hwmon",
		"unraid-vsock-hwmon.service",
	} {
		t.Run("reject self "+value, func(t *testing.T) {
			if _, err := parseRestartUnits(value); err == nil || !strings.Contains(err.Error(), "cannot restart itself") {
				t.Fatalf("self-restart unit %q returned %v", value, err)
			}
		})
	}
}

func TestPublisherReconfiguresWhenLabelChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", 34)}
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sdb)", 35)}
	reconfigured, err := publishHWMonFamily(path, "disk", &inventory, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !reconfigured {
		t.Fatal("a label change must reconfigure the hwmon family")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:serial\t35000\tdisk1 (sdb)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("update = %q, want replacement configuration %q", got, want)
	}
	if got, want := inventory.sensors[0].label, "disk1 (sdb)"; got != want {
		t.Fatalf("configured label = %q, want %q", got, want)
	}
}

func TestPublisherCachesChangedLabel(t *testing.T) {
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
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
		HBAs:  []sensors.HBA{},
	}

	reconfigured, err := publisher.publish(device, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reconfigured {
		t.Fatal("a label change must be reported as a reconfiguration")
	}
	data, err := os.ReadFile(publisher.cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cached cachedHWMonInventory
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Disks == nil || len(cached.Disks.Sensors) != 1 {
		t.Fatalf("cached disks = %#v, want one sensor", cached.Disks)
	}
	if got, want := cached.Disks.Sensors[0].Label, "disk1 (sdb)"; got != want {
		t.Fatalf("cached label = %q, want %q", got, want)
	}
}

func TestPublisherReconfiguresAfterStaleCommit(t *testing.T) {
	inventory := hwmonInventory{
		initialized: true,
		sensors:     []hwmonSensor{{id: "disk:serial", label: "disk1 (sda)"}},
	}
	current := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", 35)}
	var operations []string
	write := func(_, _, operation string, readings []hwmonSample) error {
		operations = append(operations, operation)
		if operation == "commit" {
			return syscall.ESTALE
		}
		if !reflect.DeepEqual(readings, current) {
			t.Fatalf("configured readings = %#v, want %#v", readings, current)
		}
		return nil
	}
	changed, err := publishHWMonFamilyWithWriter("unused", "disk", &inventory, current, write)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if want := []string{"commit", "configure"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
	if !reflect.DeepEqual(inventory.sensors, sensorsFromSamples(current)) {
		t.Fatalf("inventory = %#v, want %#v", inventory.sensors, sensorsFromSamples(current))
	}
}

func TestPublisherDoesNotReconfigureAfterOtherCommitError(t *testing.T) {
	inventory := hwmonInventory{
		initialized: true,
		sensors:     []hwmonSensor{{id: "disk:serial", label: "disk1"}},
	}
	current := []hwmonSample{hwmonTestSample("disk:serial", "disk1", 35)}
	calls := 0
	wantErr := errors.New("write failed")
	write := func(_, _, _ string, _ []hwmonSample) error {
		calls++
		return wantErr
	}
	changed, err := publishHWMonFamilyWithWriter("unused", "disk", &inventory, current, write)
	if changed || !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("changed=%v err=%v calls=%d", changed, err, calls)
	}
}

func TestPublisherPublishesEmptyHBAInventory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks:     hwmonInventory{initialized: true},
	}
	changed, err := publisher.publish(path, sensors.Response{Disks: []sensors.Disk{}, HBAs: []sensors.HBA{}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("configuration = %q, want %q", got, want)
	}

	publisher.hbas = hwmonInventory{
		initialized: true,
		sensors:     []hwmonSensor{{id: "hba:sas:1234", label: "HBA"}},
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed, err = publisher.publish(path, sensors.Response{Disks: []sensors.Disk{}, HBAs: []sensors.HBA{}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.hbas.sensors) != 0 {
		t.Fatalf("HBA inventory = %#v, want empty", publisher.hbas.sensors)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("cleared configuration = %q, want %q", got, want)
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
