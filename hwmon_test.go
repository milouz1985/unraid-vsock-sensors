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
	"slices"
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
	out := make(chan sensors.Response)
	done := make(chan error, 1)
	go func() { done <- receiveSnapshots(ctx, listener, expectedCID, out) }()

	waitForSnapshotAccept(t, listener)
	assertSnapshotConnectionClosed(t, wrongClient, time.Second)
	waitForSnapshotAccept(t, listener)
	want := sensors.Response{Version: "accepted", Timestamp: time.Now().UTC()}
	if err := sensors.WriteFrame(validClient, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-out:
		if got.Version != want.Version {
			t.Fatalf("received version = %q, want %q", got.Version, want.Version)
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
	out := make(chan sensors.Response)
	done := make(chan error, 1)
	go func() { done <- receiveSnapshots(ctx, listener, expectedCID, out) }()

	waitForSnapshotAccept(t, listener)
	acceptedAt := time.Now()
	assertSnapshotConnectionClosed(t, firstClient, snapshotStreamTimeout+time.Second)
	if elapsed := time.Since(acceptedAt); elapsed < snapshotStreamTimeout-100*time.Millisecond {
		t.Fatalf("silent connection closed after %v, want at least %v", elapsed, snapshotStreamTimeout)
	}

	waitForSnapshotAccept(t, listener)
	want := sensors.Response{Version: "reconnected", Timestamp: time.Now().UTC()}
	if err := sensors.WriteFrame(secondClient, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-out:
		if got.Version != want.Version {
			t.Fatalf("received version = %q, want %q", got.Version, want.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected publisher did not publish its snapshot")
	}

	cancel()
	_ = secondClient.Close()
	waitForSnapshotReceiver(t, done)
}

func TestMakeHWMonSamples(t *testing.T) {
	state := sensors.Response{
		Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
			{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
			{ID: "4", Name: "external", Device: "sdc", Transport: "usb", Rotational: true, Temp: 60},
		},
		HBAs: []sensors.HBA{{ID: "sas:1234", Model: "SAS3008", PCIAddress: "0000:06:10.0", Temp: 51}},
	}

	disks, hbas := makeHWMonSamples(state)
	wantDisks := []hwmonSample{
		hwmonTestSample("disk:group:hdd", "HDD maximum", 38, "disk:1", "disk:2"),
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

func TestHWMonGroupMembersUseStableOrder(t *testing.T) {
	first, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "2", Name: "alpha", Device: "sdb", Rotational: true, Temp: 35},
		{ID: "1", Name: "beta", Device: "sdc", Rotational: true, Temp: 36},
	}})
	second, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "alpha", Device: "sdc", Rotational: true, Temp: 36},
		{ID: "2", Name: "beta", Device: "sdb", Rotational: true, Temp: 35},
	}})
	if len(first) == 0 || len(second) == 0 || !slices.Equal(first[0].sensor.members, second[0].sensor.members) {
		t.Fatalf("group member order changed: first=%v second=%v", first[0].sensor.members, second[0].sensor.members)
	}
}

func TestPublisherFailsSafeUnavailableDiskAndItsGroup(t *testing.T) {
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
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial, false); err != nil {
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
	if _, err := publishHWMonFamily(path, "disk", &inventory, current, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:group:hdd\t100000\tHDD maximum\n" +
		"sample\tdisk:1\t35000\tdisk1 (sda)\n" +
		"sample\tdisk:2\t100000\tdisk2 (sdb)\n" +
		"sample\tdisk:3\t46000\tcache (nvme0n1)\n" +
		"commit\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("update = %q, want explicit disk and group failsafe %q", got, want)
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
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Error: "disks.ini failed",
		HBAs:  []sensors.HBA{{ID: "sas:1234", Temp: 51}},
	}
	_, err := (&hwmonPublisher{}).publish(path, state)
	if err == nil {
		t.Fatal("expected disk error")
	}
	if message := err.Error(); !strings.Contains(message, "disks: disks.ini failed") {
		t.Fatalf("unexpected error %q", message)
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
		_, _ = publishHWMonFamily(path, "disk", &publisher.disks, readings, false)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:1\t35000\tdisk1 (sda)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("update = %q, want replacement inventory %q", got, want)
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

func TestPublisherRejectsTrailingCacheData(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"version":1} {"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := (&hwmonPublisher{cachePath: cache}).restore(device); err == nil ||
		!strings.Contains(err.Error(), "unexpected data after inventory") {
		t.Fatalf("restore error = %v, want trailing data error", err)
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

func TestPublisherUsesStableIDWhenLabelChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", 34)}
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sdb)", 35)}
	if _, err := publishHWMonFamily(path, "disk", &inventory, changed, false); err != nil {
		t.Fatalf("a label change must not change sensor identity: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:serial\t35000\tdisk1 (sda)\ncommit\tdisk\n"; got != want {
		t.Fatalf("update = %q, want configured label %q", got, want)
	}
}

func TestPublisherReconfiguresAfterStaleCommit(t *testing.T) {
	inventory := hwmonInventory{
		initialized: true,
		sensors:     []hwmonSensor{{id: "disk:serial", label: "disk1 (sda)"}},
	}
	current := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sdb)", 35)}
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
	changed, err := publishHWMonFamilyWithWriter("unused", "disk", &inventory, current, false, write)
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
	changed, err := publishHWMonFamilyWithWriter("unused", "disk", &inventory, current, false, write)
	if changed || !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("changed=%v err=%v calls=%d", changed, err, calls)
	}
}

func TestPublisherWaitsForFirstNonEmptyInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	if _, err := publishHWMonFamily(path, "disk", &inventory, nil, false); err == nil {
		t.Fatal("an empty initial inventory should not be configured")
	}
	if inventory.initialized {
		t.Fatal("empty initial inventory was frozen")
	}
	readings := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", 35)}
	if _, err := publishHWMonFamily(path, "disk", &inventory, readings, false); err != nil {
		t.Fatal(err)
	}
	if !inventory.initialized {
		t.Fatal("non-empty inventory was not configured")
	}
}

func TestPublisherAllowsExplicitlyDisabledEmptyFamily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	if _, err := publishHWMonFamily(path, "hba", &inventory, nil, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("configuration = %q, want %q", got, want)
	}
}

func makeDiskSamples(state sensors.Response) []hwmonSample {
	disks, _ := makeHWMonSamples(state)
	return disks
}

func hwmonTestSample(id, label string, temperature float64, members ...string) hwmonSample {
	return hwmonSample{
		sensor: hwmonSensor{id: id, label: label, members: members}, temperature: temperature,
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
		Disks:       []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34}},
		HBADisabled: true,
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
