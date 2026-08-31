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
	internalDisks := make([]sensors.Disk, 0, len(state.Disks))
	for _, disk := range state.Disks {
		if !disk.IsExternal() {
			internalDisks = append(internalDisks, disk)
		}
	}

	for _, group := range hwmonDiskGroups {
		groupDisks := make([]sensors.Disk, 0, len(internalDisks))
		members := make([]string, 0, len(internalDisks))
		for _, disk := range internalDisks {
			if disk.Kind() == group.kind {
				groupDisks = append(groupDisks, disk)
				members = append(members, "disk:"+disk.ID)
			}
		}
		if len(groupDisks) < minHWMonGroupSize {
			continue
		}
		sort.Strings(members)
		maximum := sensors.MaxTemperature(groupDisks, func(disk sensors.Disk) float64 {
			return disk.Temp
		})
		for _, disk := range groupDisks {
			if disk.Unavailable {
				maximum = hwmonFailsafeTemp
				break
			}
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor: hwmonSensor{
				id: "disk:group:" + string(group.kind), label: group.label, members: members,
			},
			temperature: maximum,
		})
	}

	for _, disk := range internalDisks {
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
