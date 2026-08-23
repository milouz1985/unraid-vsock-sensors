package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	device "unraid-vsock-sensors/coolercontrol-plugin/gen/device_service"
	models "unraid-vsock-sensors/coolercontrol-plugin/gen/models"
)

func sampleResponse() unraidResponse {
	return unraidResponse{
		Disks: []diskReading{
			{Name: "disk1", Device: "sdb", Transport: "ata", Rotational: true, Temp: 35},
			{Name: "disk2", Device: "sdc", Transport: "ata", Rotational: true, Temp: 0},
			{Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 48},
		},
		HBAs: []hbaReading{{Name: "hba0", Temp: 49}},
	}
}

func TestGroupMaximumAndSleepingDisk(t *testing.T) {
	readings := makeStatus(sampleResponse())
	values := make(map[string]float64)
	for _, reading := range readings {
		values[reading.Id] = reading.Metric.(*models.Status_Temp).Temp
	}
	if values["hdd"] != 35 || values["disk-disk2"] != 0 || values["nvme"] != 48 {
		t.Fatalf("unexpected readings: %#v", values)
	}
}

func TestDeviceListsIndividualSensors(t *testing.T) {
	temps := makeDevice(sampleResponse(), 42, 19090).Info.Temps
	if temps["disk-disk1"] == nil || temps["hba-hba0"] == nil {
		t.Fatalf("missing individual sensors: %#v", temps)
	}
	if temps["hba"] != nil {
		t.Fatal("aggregate HBA sensor should not be exposed")
	}
	if temps["all"] != nil {
		t.Fatal("cross-family disk aggregate should not be exposed")
	}
}

func TestTransientHBAErrorKeepsCachedReading(t *testing.T) {
	state := sampleResponse()
	state.HBAError = "storcli failed"
	readings := makeStatus(state)
	for _, reading := range readings {
		if reading.Id == "hba-hba0" {
			if got := reading.Metric.(*models.Status_Temp).Temp; got != 49 {
				t.Fatalf("got HBA temperature %v, want 49", got)
			}
			return
		}
	}
	t.Fatal("cached HBA reading was hidden by a transient error")
}

func TestRealFetchErrorIsUnavailable(t *testing.T) {
	service := newUnraidService(42, 19090)
	service.fetch = func(uint32, uint32) (unraidResponse, error) {
		return unraidResponse{}, errors.New("transport failed")
	}
	_, err := service.Status(context.Background(), &device.StatusRequest{DeviceId: deviceID})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want unavailable", err)
	}
}

func TestSensorID(t *testing.T) {
	if got := sensorID("disk", "Cache Pool"); got != "disk-cache-pool" {
		t.Fatalf("got %q", got)
	}
}

func TestGRPCHealthEndpoint(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	device.RegisterDeviceServiceServer(server, newUnraidService(42, 19090))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reply, err := device.NewDeviceServiceClient(conn).Health(context.Background(), &device.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Name != serviceID || reply.Status != device.HealthResponse_STATUS_OK {
		t.Fatalf("unexpected health response: %#v", reply)
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"cid":57,"port":20000}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path, runtimeConfig{CID: 3, Port: 19090})
	if err != nil {
		t.Fatal(err)
	}
	if config.CID != 57 || config.Port != 20000 {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"cid":0,"port":19090}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, runtimeConfig{}); err == nil {
		t.Fatal("invalid CID should be rejected")
	}
}

func TestUnixSocketIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.sock")
	listener, err := listenUnixSocket(path)
	if errors.Is(err, syscall.EPERM) {
		t.Skip("Unix sockets are blocked by the test sandbox")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket permissions are %04o, want 0600", got)
	}
}

func TestUnixSocketDoesNotReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path); err == nil {
		t.Fatal("regular file should not be replaced")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "keep" {
		t.Fatalf("regular file was altered: %q, %v", content, err)
	}
}
