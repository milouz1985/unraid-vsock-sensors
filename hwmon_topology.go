package main

import (
	"fmt"
	"slices"
	"sort"

	"unraid-vsock-sensors/internal/sensors"
)

const (
	hwmonFailsafeTemp = 100.0
	minHWMonGroupSize = 2
)

type hwmonSensor struct {
	id      string
	label   string
	members []string
}

type hwmonSample struct {
	sensor      hwmonSensor
	temperature float64
}

type hwmonInventory struct {
	initialized bool
	sensors     []hwmonSensor
}

type hwmonDiskGroup struct {
	kind  sensors.DiskKind
	label string
}

var hwmonDiskGroups = []hwmonDiskGroup{
	{kind: sensors.DiskKindHDD, label: "HDD maximum"},
	{kind: sensors.DiskKindSATASSD, label: "SATA SSD maximum"},
	{kind: sensors.DiskKindNVMe, label: "NVMe SSD maximum"},
}

func makeHWMonSamples(state sensors.Response) (diskSamples, hbaSamples []hwmonSample) {
	for _, group := range hwmonDiskGroups {
		members := make([]string, 0, len(state.Disks))
		maximum := 0.0
		unavailable := false
		for _, disk := range state.Disks {
			if disk.Kind() != group.kind {
				continue
			}
			if len(members) == 0 || disk.Temp > maximum {
				maximum = disk.Temp
			}
			unavailable = unavailable || disk.Unavailable
			members = append(members, "disk:"+disk.ID)
		}
		if len(members) < minHWMonGroupSize {
			continue
		}
		sort.Strings(members)
		if unavailable {
			maximum = hwmonFailsafeTemp
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor: hwmonSensor{
				id: "disk:group:" + string(group.kind), label: group.label, members: members,
			},
			temperature: maximum,
		})
	}

	for _, disk := range state.Disks {
		temperature := disk.Temp
		if disk.Unavailable {
			temperature = hwmonFailsafeTemp
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor: hwmonSensor{
				id: "disk:" + disk.ID, label: fmt.Sprintf("%s (%s)", disk.Name, disk.Device),
			},
			temperature: temperature,
		})
	}
	for _, hba := range state.HBAs {
		label := hba.ID
		if hba.Model != "" && hba.PCIAddress != "" {
			label = fmt.Sprintf("%s (%s)", hba.Model, hba.PCIAddress)
		} else if hba.Model != "" {
			label = hba.Model
		} else if hba.PCIAddress != "" {
			label = hba.PCIAddress
		}
		hbaSamples = append(hbaSamples, hwmonSample{
			sensor:      hwmonSensor{id: "hba:" + hba.ID, label: label},
			temperature: hba.Temp,
		})
	}
	return diskSamples, hbaSamples
}

func sameHWMonTopology(expected []hwmonSensor, current []hwmonSample) bool {
	if len(expected) != len(current) {
		return false
	}
	currentByID := make(map[string]hwmonSensor, len(current))
	for _, sample := range current {
		currentByID[sample.sensor.id] = sample.sensor
	}
	for _, reading := range expected {
		other, found := currentByID[reading.id]
		if !found || !slices.Equal(reading.members, other.members) {
			return false
		}
	}
	return true
}

func sensorsFromSamples(samples []hwmonSample) []hwmonSensor {
	sensors := make([]hwmonSensor, 0, len(samples))
	for _, sample := range samples {
		sensor := sample.sensor
		sensor.members = append([]string(nil), sensor.members...)
		sensors = append(sensors, sensor)
	}
	return sensors
}
