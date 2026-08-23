package main

import (
	"context"
	"fmt"
	"log"
	"time"

	device "unraid-vsock-sensors/coolercontrol-plugin/gen/device_service"
	models "unraid-vsock-sensors/coolercontrol-plugin/gen/models"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const deviceID = "unraid-storage"

type unraidService struct {
	device.UnimplementedDeviceServiceServer
	cid     uint32
	port    uint32
	started time.Time
	fetch   func(uint32, uint32) (unraidResponse, error)
}

func newUnraidService(cid, port uint32) *unraidService {
	return &unraidService{cid: cid, port: port, started: time.Now(), fetch: fetchUnraid}
}

func (s *unraidService) Health(context.Context, *device.HealthRequest) (*device.HealthResponse, error) {
	return &device.HealthResponse{
		Name:          serviceID,
		Version:       version,
		Status:        device.HealthResponse_STATUS_OK,
		UptimeSeconds: uint64(time.Since(s.started).Seconds()),
	}, nil
}

func (s *unraidService) ListDevices(context.Context, *device.ListDevicesRequest) (*device.ListDevicesResponse, error) {
	state, err := s.fetch(s.cid, s.port)
	if err != nil {
		log.Printf("initial Unraid discovery failed: %v", err)
	}
	return &device.ListDevicesResponse{Devices: []*models.Device{makeDevice(state, s.cid, s.port)}}, nil
}

func (s *unraidService) InitializeDevice(context.Context, *device.InitializeDeviceRequest) (*device.InitializeDeviceResponse, error) {
	return &device.InitializeDeviceResponse{}, nil
}

func (s *unraidService) Shutdown(context.Context, *device.ShutdownRequest) (*device.ShutdownResponse, error) {
	return &device.ShutdownResponse{}, nil
}

func (s *unraidService) Status(_ context.Context, request *device.StatusRequest) (*device.StatusResponse, error) {
	if request.DeviceId != deviceID {
		return nil, status.Error(codes.NotFound, "unknown device")
	}
	state, err := s.fetch(s.cid, s.port)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if state.DiskError != "" {
		return nil, status.Error(codes.Unavailable, state.DiskError)
	}
	return &device.StatusResponse{Status: makeStatus(state)}, nil
}

func makeDevice(state unraidResponse, cid, port uint32) *models.Device {
	temps := make(map[string]*models.TempInfo)
	number := uint32(1)
	groups := []struct{ id, label string }{
		{"hdd", "HDD maximum"},
		{"ssd", "SSD maximum"},
		{"nvme", "NVMe maximum"},
	}
	for _, group := range groups {
		temps[group.id] = &models.TempInfo{Label: group.label, Number: number}
		number++
	}
	for _, disk := range state.Disks {
		temps[sensorID("disk", disk.Name)] = &models.TempInfo{
			Label: fmt.Sprintf("%s (%s)", disk.Name, disk.Device), Number: number,
		}
		number++
	}
	for _, hba := range state.HBAs {
		temps[sensorID("hba", hba.Name)] = &models.TempInfo{Label: hba.Name, Number: number}
		number++
	}

	location := fmt.Sprintf("vsock:%d:%d", cid, port)
	name, model, driver, uid := "Unraid Storage", "Unraid disks and HBAs over AF_VSOCK", "unraid-vsock-sensors", location
	minTemp, maxTemp := 0.0, 100.0
	return &models.Device{
		Id: deviceID, Name: name, UidInfo: &uid,
		Info: &models.DeviceInfo{
			Channels: map[string]*models.ChannelInfo{}, Temps: temps,
			TempMin: &minTemp, TempMax: &maxTemp, Model: &model,
			DriverInfo: &models.DriverInfo{Name: &driver, Version: stringPointer(version), Locations: []string{location}},
		},
	}
}

func makeStatus(state unraidResponse) []*models.Status {
	var result []*models.Status
	addDiskMaximum := func(id string, match func(diskReading) bool) {
		var maximum float64
		found := false
		for _, disk := range state.Disks {
			if match(disk) && (!found || disk.Temp > maximum) {
				maximum, found = disk.Temp, true
			}
		}
		if found {
			result = append(result, tempStatus(id, maximum))
		}
	}
	addDiskMaximum("hdd", func(d diskReading) bool { return d.Rotational })
	addDiskMaximum("ssd", isSSD)
	addDiskMaximum("nvme", isNVMe)
	for _, disk := range state.Disks {
		result = append(result, tempStatus(sensorID("disk", disk.Name), disk.Temp))
	}
	if state.HBAError == "" {
		for _, hba := range state.HBAs {
			result = append(result, tempStatus(sensorID("hba", hba.Name), hba.Temp))
		}
	}
	return result
}

func tempStatus(id string, temperature float64) *models.Status {
	return &models.Status{Id: id, Metric: &models.Status_Temp{Temp: temperature}}
}

func stringPointer(value string) *string { return &value }
