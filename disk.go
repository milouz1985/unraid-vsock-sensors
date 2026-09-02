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

type diskState uint8

const (
	diskStateInvalid diskState = iota
	diskStateAvailable
	diskStateStandby
	diskStatePending
	diskStateUnavailable
)

// unraidDisk is the raw disk state read from Unraid's disks.ini. Transient
// Unraid states deliberately stay out of the VSOCK sensor model.
type unraidDisk struct {
	id, name, device, transport string
	rotational                  bool
	temp                        float64
	state                       diskState
}

func (d unraidDisk) sensor(temp float64, unavailable bool) sensors.Disk {
	return sensors.Disk{
		ID: d.id, Name: d.name, Device: d.device, Transport: d.transport,
		Rotational: d.rotational, Temp: temp, Unavailable: unavailable,
	}
}

func newDiskReader(path string, grace time.Duration) *diskReader {
	return &diskReader{
		path: path, grace: grace, lastValid: make(map[string]float64),
		pendingSince: make(map[string]time.Time), now: time.Now,
	}
}

func (r *diskReader) read() ([]sensors.Disk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rawDisks, err := readDisks(r.path)
	if err != nil {
		return nil, err
	}
	now := r.now()
	present := make(map[string]struct{}, len(rawDisks))
	disks := make([]sensors.Disk, 0, len(rawDisks))
	for _, disk := range rawDisks {
		present[disk.id] = struct{}{}
		temperature, unavailable := disk.temp, false
		switch disk.state {
		case diskStatePending:
			started, ok := r.pendingSince[disk.id]
			if !ok {
				started = now
				r.pendingSince[disk.id] = started
			}
			if now.Sub(started) < r.grace {
				// Without a previous reading, 0°C is intentionally published as
				// available during the grace period. Marking it unavailable would
				// make hwmon apply its 100°C failsafe while the disk spins up.
				temperature = r.lastValid[disk.id]
			} else {
				unavailable = true
			}
		case diskStateStandby:
			delete(r.pendingSince, disk.id)
		case diskStateAvailable:
			r.lastValid[disk.id] = disk.temp
			delete(r.pendingSince, disk.id)
		case diskStateInvalid, diskStateUnavailable:
			unavailable = true
			delete(r.pendingSince, disk.id)
		}
		disks = append(disks, disk.sensor(temperature, unavailable))
	}
	for id := range r.lastValid {
		if _, ok := present[id]; !ok {
			delete(r.lastValid, id)
		}
	}
	for id := range r.pendingSince {
		if _, ok := present[id]; !ok {
			delete(r.pendingSince, id)
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
func readDisks(disksINIPath string) ([]unraidDisk, error) {
	config, err := ini.Load(disksINIPath)
	if err != nil {
		return nil, err
	}

	var disks []unraidDisk
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
		state := diskStateAvailable
		temp := 0.0
		if rawTemp == "*" {
			// Report a spun-down disk as 0°C instead of omitting its sensor.
			// CoolerControl treats repeated missing readings as a sensor failure and
			// eventually substitutes its 100°C failsafe value, which could drive a
			// fan curve to maximum while the disk is intentionally asleep.
			if strings.TrimSpace(section.Key("spundown").String()) == "1" {
				state = diskStateStandby
			} else {
				state = diskStatePending
			}
		} else {
			temp, err = strconv.ParseFloat(rawTemp, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) || temp < 0 || temp > 150 {
				temp = 0
				state = diskStateUnavailable
			}
		}
		// Unraid writes 1 for rotational disks and 0 for solid-state disks.
		rotational := strings.TrimSpace(section.Key("rotational").String()) == "1"
		disks = append(disks, unraidDisk{
			id: id, name: name, device: device,
			transport:  strings.ToLower(section.Key("transport").String()),
			rotational: rotational, temp: temp, state: state,
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].name < disks[j].name })
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
