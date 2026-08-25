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
	"unraid-vsock-sensors/internal/sensors"
)

func sampleResponse() sensors.Response {
	return sensors.Response{
		Version: "server-test-version",
		Disks: []sensors.Disk{
			{Name: "disk1", Device: "sdb", Transport: "ata", Rotational: true, Temp: 35},
			{Name: "disk2", Device: "sdc", Transport: "ata", Rotational: true, Temp: 0},
			{Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 48},
		},
		HBAs: []sensors.HBA{{Name: "hba0", Temp: 49}},
	}
}

func TestGroupMaximumAndSleepingDisk(t *testing.T) {
	readings := makeStatus(sampleResponse())
	values := make(map[string]float64)
	for _, reading := range readings {
		values[reading.Id] = reading.Metric.(*models.Status_Temp).Temp
	}
	if values["hdd"] != 35 || values["disk-disk2"] != 0 || values["disk-cache"] != 48 {
		t.Fatalf("unexpected readings: %#v", values)
	}
	if _, exists := values["nvme"]; exists {
		t.Fatal("single NVMe should not have an aggregate sensor")
	}
}

func TestDeviceListsIndividualSensors(t *testing.T) {
	device := makeDevice(sampleResponse(), 42, 19090)
	temps := device.Info.Temps
	if temps["disk-disk1"] == nil || temps["hba-hba0"] == nil {
		t.Fatalf("missing individual sensors: %#v", temps)
	}
	if temps["hba"] != nil {
		t.Fatal("aggregate HBA sensor should not be exposed")
	}
	if temps["all"] != nil {
		t.Fatal("cross-family disk aggregate should not be exposed")
	}
	if temps["hdd"] == nil || temps["nvme"] != nil || temps["ssd"] != nil {
		t.Fatalf("unexpected family aggregates: %#v", temps)
	}
	if device.Info.DriverInfo.Version == nil || *device.Info.DriverInfo.Version != "server-test-version" {
		t.Fatalf("unexpected server version: %#v", device.Info.DriverInfo.Version)
	}
}

func TestDeviceOmitsUnknownServerVersion(t *testing.T) {
	state := sampleResponse()
	state.Version = ""
	if version := makeDevice(state, 42, 19090).Info.DriverInfo.Version; version != nil {
		t.Fatalf("unexpected server version: %q", *version)
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
	service.fetch = func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{}, errors.New("transport failed")
	}
	_, err := service.Status(context.Background(), &device.StatusRequest{DeviceId: deviceID})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want unavailable", err)
	}
}

func TestDiscoveryErrorDoesNotRegisterEmptyDevice(t *testing.T) {
	for name, fetch := range map[string]func(context.Context, uint32, uint32) (sensors.Response, error){
		"transport": func(context.Context, uint32, uint32) (sensors.Response, error) {
			return sensors.Response{}, errors.New("transport failed")
		},
		"disks": func(context.Context, uint32, uint32) (sensors.Response, error) {
			return sensors.Response{Error: "disks.ini failed"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			service := newUnraidService(42, 19090)
			service.fetch = fetch
			_, err := service.ListDevices(context.Background(), &device.ListDevicesRequest{})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("got %v, want unavailable", err)
			}
		})
	}
}

func TestDiskErrorDoesNotBlockHBAStatus(t *testing.T) {
	service := newUnraidService(42, 19090)
	service.fetch = func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{
			Error: "disks.ini failed",
			HBAs:  []sensors.HBA{{Name: "hba0", Temp: 46}},
		}, nil
	}
	reply, err := service.Status(context.Background(), &device.StatusRequest{DeviceId: deviceID})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Status) != 1 || reply.Status[0].Id != "hba-hba0" {
		t.Fatalf("unexpected status: %#v", reply.Status)
	}
	if got := reply.Status[0].Metric.(*models.Status_Temp).Temp; got != 46 {
		t.Fatalf("got HBA temperature %v, want 46", got)
	}
}

func TestStatusFailsWhenNoSourceIsAvailable(t *testing.T) {
	for name, state := range map[string]sensors.Response{
		"disks": {Error: "disks.ini failed"},
		"HBA":   {HBAError: "storcli failed"},
		"both":  {Error: "disks.ini failed", HBAError: "storcli failed"},
	} {
		t.Run(name, func(t *testing.T) {
			service := newUnraidService(42, 19090)
			service.fetch = func(context.Context, uint32, uint32) (sensors.Response, error) {
				return state, nil
			}
			_, err := service.Status(context.Background(), &device.StatusRequest{DeviceId: deviceID})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("got %v, want unavailable", err)
			}
		})
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
	if reply.Name != serviceID || reply.Version != version || reply.Status != device.HealthResponse_STATUS_OK {
		t.Fatalf("unexpected health response: %#v", reply)
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"cid":57,"port":20000}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.CID != 57 || config.Port != 20000 {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for name, config := range map[string]string{
		"CID below guest range": `{"cid":0,"port":19090}`,
		"CID any":               `{"cid":4294967295,"port":19090}`,
		"zero port":             `{"cid":42,"port":0}`,
		"port any":              `{"cid":42,"port":4294967295}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path); err == nil {
				t.Fatal("invalid configuration should be rejected")
			}
		})
	}
}

func TestLoadConfigUsesDefaultsWhenMissing(t *testing.T) {
	config, err := loadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config != defaultConfig {
		t.Fatalf("got %#v, want %#v", config, defaultConfig)
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
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("socket permissions %04o allow group or other access", got)
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
