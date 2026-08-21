package main

import (
	"sort"
	"strings"

	"gopkg.in/ini.v1"
)

type disk struct {
	Name       string  `json:"name"`
	Device     string  `json:"device"`
	Transport  string  `json:"transport,omitempty"`
	Rotational bool    `json:"rotational"`
	Temp       float64 `json:"temp_c"`
}

// readDisks reads the temperatures already cached by Unraid in disks.ini.
// It does not call smartctl and therefore does not wake sleeping disks.
func readDisks(path string) ([]disk, error) {
	config, err := ini.Load(path)
	if err != nil {
		return nil, err
	}

	var result []disk
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")

		temp, err := section.Key("temp").Float64()
		if err != nil {
			// Unraid uses "*" when a sleeping disk has no temperature.
			continue
		}
		rotational, _ := section.Key("rotational").Bool()
		result = append(result, disk{
			Name:       name,
			Device:     section.Key("device").String(),
			Transport:  strings.ToLower(section.Key("transport").String()),
			Rotational: rotational,
			Temp:       temp,
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func selectDisks(disks []disk, selector string) []disk {
	selector = strings.ToLower(selector)
	var result []disk

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
