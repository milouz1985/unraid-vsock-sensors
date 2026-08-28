package main

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

// readDisks reads the temperatures already cached by Unraid in disks.ini.
// It does not call smartctl and therefore does not wake sleeping disks.
func readDisks(disksINIPath string) ([]sensors.Disk, error) {
	config, err := ini.Load(disksINIPath)
	if err != nil {
		return nil, err
	}

	var disks []sensors.Disk
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")
		id := strings.TrimSpace(section.Key("id").String())
		device := strings.TrimSpace(section.Key("device").String())
		status := strings.TrimSpace(section.Key("status").String())
		// disks.ini contains sections for every possible array slot, including
		// unassigned DISK_NP entries, plus the Unraid boot flash device.
		if id == "" || device == "" || strings.EqualFold(status, "DISK_NP") || strings.EqualFold(name, "flash") {
			continue
		}

		rawTemp := strings.TrimSpace(section.Key("temp").String())
		temp := 0.0
		unavailable := false
		if rawTemp == "*" {
			if strings.TrimSpace(section.Key("spundown").String()) != "1" {
				unavailable = true
			}
			// Report a spun-down disk as 0°C instead of omitting its sensor.
			// CoolerControl treats repeated missing readings as a sensor failure and
			// eventually substitutes its 100°C failsafe value, which could drive a
			// fan curve to maximum while the disk is intentionally asleep.
		} else {
			temp, err = strconv.ParseFloat(rawTemp, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) || temp < 0 || temp > 150 {
				temp = 0
				unavailable = true
			}
		}
		// Unraid writes rotational as 0 or 1 in disks.ini, so trust its value.
		rotational, _ := section.Key("rotational").Bool()
		disks = append(disks, sensors.Disk{
			ID:          id,
			Name:        name,
			Device:      device,
			Transport:   strings.ToLower(section.Key("transport").String()),
			Rotational:  rotational,
			Temp:        temp,
			Unavailable: unavailable,
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

func selectDisks(disks []sensors.Disk, selector string, availableOnly bool) []sensors.Disk {
	selector = strings.ToLower(selector)
	var matches []sensors.Disk

	for _, disk := range disks {
		if availableOnly && disk.Unavailable {
			continue
		}
		var match bool
		switch selector {
		case "all":
			match = true
		case "nvme":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindNVMe
		case "hdd":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindHDD
		case "ssd":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindSATASSD
		default:
			match = strings.EqualFold(disk.Name, selector) || strings.EqualFold(disk.Device, selector)
		}
		if match {
			matches = append(matches, disk)
		}
	}

	return matches
}
