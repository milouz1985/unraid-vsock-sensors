package main

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	device "unraid-vsock-sensors/coolercontrol-plugin/gen/device_service"
	models "unraid-vsock-sensors/coolercontrol-plugin/gen/models"
	"unraid-vsock-sensors/internal/sensors"
)

func sampleResponse() sensors.Response {
	return sensors.Response{
		Version: "server-test-version",
		Disks: []sensors.Disk{
			{ID: "serial-disk1", Name: "disk1", Device: "sdb", Transport: "ata", Rotational: true, Temp: 35},
			{ID: "serial-disk2", Name: "disk2", Device: "sdc", Transport: "ata", Rotational: true, Temp: 0},
			{ID: "serial-cache", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 48},
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
	if values["hdd"] != 35 || values["disk-serial-disk2"] != 0 || values["disk-serial-cache"] != 48 {
		t.Fatalf("unexpected readings: %#v", values)
	}
	if _, exists := values["nvme"]; exists {
		t.Fatal("single NVMe should not have an aggregate sensor")
	}
}

func TestDeviceListsIndividualSensors(t *testing.T) {
	device := makeDevice(sampleResponse(), 42, 19090)
	temps := device.Info.Temps
	if temps["disk-serial-disk1"] == nil || temps["hba-hba0"] == nil {
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

func TestHealth(t *testing.T) {
	reply, err := newUnraidService(42, 19090).Health(context.Background(), &device.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Name != serviceID || reply.Version != version || reply.Status != device.HealthResponse_STATUS_OK {
		t.Fatalf("unexpected health response: %#v", reply)
	}
}
