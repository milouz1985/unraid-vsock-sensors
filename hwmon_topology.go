package main

import (
	"fmt"

	"unraid-vsock-sensors/internal/sensors"
)

const (
	hwmonFailsafeTemp = 100.0
	minHWMonGroupSize = 2
)

type hwmonSensor struct {
	id    string
	label string
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
		count := 0
		maximum := 0.0
		unavailable := false
		for _, disk := range state.Disks {
			if disk.Kind() != group.kind {
				continue
			}
			if count == 0 || disk.Temp > maximum {
				maximum = disk.Temp
			}
			unavailable = unavailable || disk.Unavailable
			count++
		}
		if count < minHWMonGroupSize {
			continue
		}
		if unavailable {
			maximum = hwmonFailsafeTemp
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor:      hwmonSensor{id: "disk:group:" + string(group.kind), label: group.label},
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

func sameHWMonConfiguration(expected []hwmonSensor, current []hwmonSample) bool {
	if len(expected) != len(current) {
		return false
	}
	currentLabels := make(map[string]string, len(current))
	for _, sample := range current {
		currentLabels[sample.sensor.id] = sample.sensor.label
	}
	for _, sensor := range expected {
		label, found := currentLabels[sensor.id]
		if !found || label != sensor.label {
			return false
		}
	}
	return true
}

func sensorsFromSamples(samples []hwmonSample) []hwmonSensor {
	sensors := make([]hwmonSensor, 0, len(samples))
	for _, sample := range samples {
		sensors = append(sensors, sample.sensor)
	}
	return sensors
}
