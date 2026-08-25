package main

import (
	"strings"

	"unraid-vsock-sensors/internal/sensors"
)

func sensorID(prefix, id string) string {
	return prefix + "-" + id
}

func isNVMe(d sensors.Disk) bool {
	return strings.EqualFold(d.Transport, "nvme") || strings.HasPrefix(d.Device, "nvme")
}

func isSSD(d sensors.Disk) bool {
	return !d.Rotational && !isNVMe(d)
}
