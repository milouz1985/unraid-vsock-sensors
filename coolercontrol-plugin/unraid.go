package main

import (
	"strings"

	"unraid-vsock-sensors/internal/sensors"
)

func sensorID(prefix, name string) string {
	var id strings.Builder
	id.WriteString(prefix)
	id.WriteByte('-')
	for _, char := range strings.ToLower(name) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			id.WriteRune(char)
		} else {
			id.WriteByte('-')
		}
	}
	return id.String()
}

func diskSensorID(disk sensors.Disk) string {
	return "disk-" + disk.ID
}

func isNVMe(d sensors.Disk) bool {
	return strings.EqualFold(d.Transport, "nvme") || strings.HasPrefix(d.Device, "nvme")
}

func isSSD(d sensors.Disk) bool {
	return !d.Rotational && !isNVMe(d)
}
