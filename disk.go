package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

const (
	diskPollMargin             = 5 * time.Second
	defaultDiskSpinupGrace     = 2 * time.Minute
	maximumRecommendedDiskPoll = 5 * time.Minute
)

type diskReader struct {
	path         string
	grace        time.Duration
	mu           sync.Mutex
	lastValid    map[string]float64
	pendingSince map[string]time.Time
	now          func() time.Time
}

func newDiskReader(path string, grace time.Duration) *diskReader {
	return &diskReader{path: path, grace: grace, lastValid: make(map[string]float64), pendingSince: make(map[string]time.Time), now: time.Now}
}

func (r *diskReader) read() ([]sensors.Disk, error) {
	disks, err := readDisks(r.path)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for i := range disks {
		disk := &disks[i]
		switch {
		case disk.Pending:
			started, ok := r.pendingSince[disk.ID]
			if !ok {
				started = now
				r.pendingSince[disk.ID] = started
			}
			if now.Sub(started) < r.grace {
				disk.Temp = r.lastValid[disk.ID]
				disk.Unavailable = false
			}
		case disk.Standby:
			delete(r.pendingSince, disk.ID)
		case !disk.Unavailable:
			r.lastValid[disk.ID] = disk.Temp
			delete(r.pendingSince, disk.ID)
		default:
			delete(r.pendingSince, disk.ID)
		}
	}
	return disks, nil
}

func diskPollingIntervals(configPath string) (poll, grace time.Duration, err error) {
	file, err := os.Open(configPath)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "poll_attributes=") {
			continue
		}
		value := strings.Trim(strings.TrimPrefix(line, "poll_attributes="), `"`)
		seconds, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("invalid poll_attributes %q: %w", value, parseErr)
		}
		poll = time.Duration(seconds) * time.Second
		break
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if poll == 0 {
		return poll, defaultDiskSpinupGrace, nil
	}
	return poll, poll + diskPollMargin, nil
}

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
			Standby:     rawTemp == "*" && strings.TrimSpace(section.Key("spundown").String()) == "1",
			Pending:     rawTemp == "*" && strings.TrimSpace(section.Key("spundown").String()) != "1",
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

func selectDisks(disks []sensors.Disk, selector string) []sensors.Disk {
	selector = strings.ToLower(selector)
	var matches []sensors.Disk

	for _, disk := range disks {
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
