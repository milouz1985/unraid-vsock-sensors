package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

// readDisks reads the temperatures already cached by Unraid in disks.ini.
// It does not call smartctl and therefore does not wake sleeping disks.
func readDisks(path string) ([]sensors.Disk, error) {
	config, err := ini.Load(path)
	if err != nil {
		return nil, err
	}

	var result []sensors.Disk
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
		if rawTemp == "*" {
			if strings.TrimSpace(section.Key("spundown").String()) != "1" {
				return nil, fmt.Errorf("disk %q temperature unavailable while not spun down", name)
			}
			// Report a spun-down disk as 0°C instead of omitting its sensor.
			// CoolerControl treats repeated missing readings as a sensor failure and
			// eventually substitutes its 100°C failsafe value, which could drive a
			// fan curve to maximum while the disk is intentionally asleep.
		} else {
			temp, err = strconv.ParseFloat(rawTemp, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) {
				return nil, fmt.Errorf("disk %q has invalid temperature %q", name, rawTemp)
			}
		}
		// Unraid writes rotational as 0 or 1 in disks.ini, so trust its value.
		rotational, _ := section.Key("rotational").Bool()
		result = append(result, sensors.Disk{
			ID:         id,
			Name:       name,
			Device:     device,
			Transport:  strings.ToLower(section.Key("transport").String()),
			Rotational: rotational,
			Temp:       temp,
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func selectDisks(disks []sensors.Disk, selector string) []sensors.Disk {
	selector = strings.ToLower(selector)
	var result []sensors.Disk

	for _, disk := range disks {
		match := selector == "all" || strings.EqualFold(disk.Name, selector) || strings.EqualFold(disk.Device, selector)
		switch selector {
		case "nvme":
			match = disk.Transport == "nvme" || strings.HasPrefix(disk.Device, "nvme")
		case "hdd":
			match = disk.Rotational
		case "ssd":
			match = !disk.Rotational && disk.Transport != "nvme" && !strings.HasPrefix(disk.Device, "nvme")
		}
		if match {
			result = append(result, disk)
		}
	}

	return result
}
