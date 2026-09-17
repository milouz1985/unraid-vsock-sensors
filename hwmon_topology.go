// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

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
	sensor       hwmonSensor
	temperature  float64
	omitOnCommit bool
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
		// If any member is unavailable, the true maximum is unknown. Keep the
		// group in the topology but stop refreshing it so virt_temp applies its
		// stale timeout. A configure still starts it at the failsafe temperature.
		if unavailable {
			maximum = hwmonFailsafeTemp
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor:       hwmonSensor{id: "disk:group:" + string(group.kind), label: group.label},
			temperature:  maximum,
			omitOnCommit: unavailable,
		})
	}

	for _, disk := range state.Disks {
		temperature := disk.Temp
		if disk.Unavailable {
			temperature = hwmonFailsafeTemp
		}
		id := "disk:" + disk.ID
		diskSamples = append(diskSamples, hwmonSample{
			sensor: hwmonSensor{
				id: id, label: sanitizeHWMonLabel(disk.Name, id),
			},
			temperature:  temperature,
			omitOnCommit: disk.Unavailable,
		})
	}
	for _, hba := range state.HBAs {
		id := "hba:" + hba.ID
		label := hba.ID
		if hba.Model != "" && hba.PCIAddress != "" {
			label = fmt.Sprintf("%s (%s)", hba.Model, hba.PCIAddress)
		} else if hba.Model != "" {
			label = hba.Model
		} else if hba.PCIAddress != "" {
			label = hba.PCIAddress
		}
		hbaSamples = append(hbaSamples, hwmonSample{
			sensor:      hwmonSensor{id: id, label: sanitizeHWMonLabel(label, id)},
			temperature: hba.Temp,
		})
	}
	return diskSamples, hbaSamples
}

// sanitizeHWMonLabel keeps presentation metadata from invalidating an otherwise
// usable temperature sample. The virt_temp text protocol reserves control
// characters and limits labels to maxHWMonLabelSize bytes. Keep the encoder's
// strict validation as a final guard, but normalize dynamic labels before they
// reach it.
func sanitizeHWMonLabel(label, fallback string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)
	label = strings.TrimSpace(label)
	if label == "" {
		label = fallback
	}
	if len(label) <= maxHWMonLabelSize {
		return label
	}

	limit := maxHWMonLabelSize
	for limit > 0 && !utf8.RuneStart(label[limit]) {
		limit--
	}
	return label[:limit]
}

// Labels are configuration data: virt_temp commit updates temperatures only,
// so changing a label requires a full configure operation.
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
