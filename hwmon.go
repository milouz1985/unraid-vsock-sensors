package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultHWMonInterval = time.Second
	virtTempDevicePath   = "/dev/virt-temp"
	defaultHWMonCache    = "/var/lib/unraid-vsock-sensors/hwmon-inventory.json"
	hwmonFailsafeTemp    = 100.0
	minHWMonGroupSize    = 2
	maxHWMonIDSize       = 63
	maxHWMonLabelSize    = 95
)

type hwmonReading struct {
	id          string
	label       string
	temperature float64
	members     []string
	unavailable bool
}

type hwmonInventory struct {
	initialized bool
	readings    []hwmonReading
}

type hwmonPublisher struct {
	disks        hwmonInventory
	hbas         hwmonInventory
	cachePath    string
	restartUnits []string
	cacheDirty   bool
}

type cachedHWMonInventory struct {
	Version int                `json:"version"`
	Disks   *cachedHWMonFamily `json:"disks,omitempty"`
	HBAs    *cachedHWMonFamily `json:"hbas,omitempty"`
}

type cachedHWMonFamily struct {
	Readings []cachedHWMonReading `json:"readings"`
}

type cachedHWMonReading struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Members []string `json:"members,omitempty"`
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

func makeHWMonReadings(state sensors.Response) (diskReadings, hbaReadings []hwmonReading) {
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
		// A maximum is useful only for a real group; with one disk it would
		// duplicate the individual sensor under another name.
		if len(groupDisks) < minHWMonGroupSize {
			continue
		}
		maximum, available := sensors.MaxAvailableDiskTemperature(groupDisks)
		diskReadings = append(diskReadings, hwmonReading{
			id:          "disk:group:" + string(group.kind),
			label:       group.label,
			members:     members,
			unavailable: !available,
			temperature: maximum,
		})
	}

	for _, disk := range internalDisks {
		diskReadings = append(diskReadings, hwmonReading{
			id:          "disk:" + disk.ID,
			label:       fmt.Sprintf("%s (%s)", disk.Name, disk.Device),
			temperature: disk.Temp,
			unavailable: disk.Unavailable,
		})
	}
	for _, hba := range state.HBAs {
		id := hba.ID
		if id == "" {
			id = hba.Name
		}
		label := hba.Name
		if hba.Model != "" {
			label += " (" + hba.Model + ")"
		}
		hbaReadings = append(hbaReadings, hwmonReading{
			id:          "hba:" + id,
			label:       label,
			temperature: hba.Temp,
		})
	}
	return diskReadings, hbaReadings
}

func hwmon(args []string) error {
	fs := flag.NewFlagSet("hwmon", flag.ContinueOnError)
	cid := fs.Uint("cid", 3, "guest vsock CID")
	port := fs.Uint("port", defaultPort, "vsock port")
	interval := fs.Duration("interval", defaultHWMonInterval, "temperature update interval")
	device := fs.String("device", virtTempDevicePath, "virt-temp control device")
	cache := fs.String("cache", defaultHWMonCache, "persistent hwmon inventory cache")
	restartUnitsFlag := fs.String("restart-units", "", "comma-separated systemd units restarted after a topology change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("hwmon does not accept positional arguments")
	}
	if err := vsockaddr.ValidateCID(uint64(*cid)); err != nil {
		return err
	}
	if err := vsockaddr.ValidatePort(uint64(*port)); err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("interval must be greater than zero")
	}
	if strings.TrimSpace(*cache) == "" {
		return errors.New("cache path must not be empty")
	}
	restartUnits, err := parseRestartUnits(*restartUnitsFlag)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("publishing fixed Unraid hwmon inventories through %s every %s", *device, *interval)
	publisher := &hwmonPublisher{cachePath: *cache, restartUnits: restartUnits}
	restored, err := publisher.restore(*device)
	if err != nil {
		log.Printf("hwmon inventory cache warning: %s", err)
	} else if restored {
		if err := restartSystemdUnits(ctx, publisher.restartUnits); err != nil {
			log.Printf("topology consumer restart warning: %s", err)
		} else if len(publisher.restartUnits) != 0 {
			log.Printf("restarted topology consumers after cache restore: %s", strings.Join(publisher.restartUnits, ", "))
		}
	}
	lastError := ""
	for {
		reconfigured, err := publisher.publish(ctx, uint32(*cid), uint32(*port), *device, sensors.Fetch)
		if reconfigured {
			if restartErr := restartSystemdUnits(ctx, publisher.restartUnits); restartErr != nil {
				log.Printf("topology consumer restart warning: %s", restartErr)
			} else if len(publisher.restartUnits) != 0 {
				log.Printf("restarted topology consumers: %s", strings.Join(publisher.restartUnits, ", "))
			}
		}
		if err != nil && err.Error() != lastError {
			lastError = err.Error()
			log.Printf("hwmon update warning; unavailable sensors will apply their failsafe: %s", lastError)
		} else if err == nil && lastError != "" {
			log.Printf("hwmon updates recovered")
			lastError = ""
		}

		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (publisher *hwmonPublisher) publish(
	parent context.Context,
	cid uint32,
	port uint32,
	device string,
	fetch func(context.Context, uint32, uint32) (sensors.Response, error),
) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	state, err := fetch(ctx, cid, port)
	if err != nil {
		return false, err
	}
	disks, hbas := makeHWMonReadings(state)
	var diskErr, hbaErr error
	reconfigured := false
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if changed, err := publishHWMonFamily(device, "disk", &publisher.disks, disks, false); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured storage hwmon inventory with %d channels", len(disks))
		}
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if changed, err := publishHWMonFamily(device, "hba", &publisher.hbas, hbas, state.HBADisabled); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	} else {
		reconfigured = reconfigured || changed
		if changed {
			log.Printf("configured HBA hwmon inventory with %d channels", len(hbas))
		}
	}
	if reconfigured {
		publisher.cacheDirty = true
	}
	if publisher.cacheDirty {
		if err := publisher.saveCache(); err != nil {
			// The kernel inventory has already changed. Notify consumers even if
			// persistence failed, and keep cacheDirty set so the next cycle retries.
			return reconfigured, errors.Join(diskErr, hbaErr, fmt.Errorf("save hwmon inventory cache: %w", err))
		}
		publisher.cacheDirty = false
	}
	return reconfigured, errors.Join(diskErr, hbaErr)
}

func publishHWMonFamily(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonReading,
	allowEmpty bool,
) (bool, error) {
	if len(current) == 0 && !allowEmpty {
		return false, errors.New("inventory is empty; waiting for sensors")
	}
	if !inventory.initialized {
		if err := writeHWMonReadings(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.initialized = true
		inventory.readings = append([]hwmonReading(nil), current...)
		return true, nil
	}
	if !sameHWMonTopology(inventory.readings, current) {
		if err := writeHWMonReadings(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.readings = append([]hwmonReading(nil), current...)
		return true, nil
	}

	currentByID := make(map[string]hwmonReading, len(current))
	for _, reading := range current {
		currentByID[reading.id] = reading
	}
	updates := make([]hwmonReading, 0, len(inventory.readings))
	for _, expected := range inventory.readings {
		reading, found := currentByID[expected.id]
		if !found || reading.unavailable {
			continue
		}
		complete := true
		for _, member := range expected.members {
			if _, found := currentByID[member]; !found {
				complete = false
				break
			}
		}
		if complete {
			// Labels describe the fixed inventory and may contain volatile names
			// such as /dev/sdX or a StorCLI index. Keep the configured label and
			// use only the stable ID to associate a new temperature.
			reading.label = expected.label
			updates = append(updates, reading)
		}
	}
	if err := writeHWMonReadings(path, namespace, "commit", updates); err != nil {
		return false, err
	}
	return false, nil
}

func sameHWMonTopology(expected, current []hwmonReading) bool {
	if len(expected) != len(current) {
		return false
	}
	currentByID := make(map[string]hwmonReading, len(current))
	for _, reading := range current {
		currentByID[reading.id] = reading
	}
	for _, reading := range expected {
		other, found := currentByID[reading.id]
		if !found || !slices.Equal(reading.members, other.members) {
			return false
		}
	}
	return true
}

func (publisher *hwmonPublisher) restore(device string) (bool, error) {
	if err := os.Chmod(publisher.cachePath, 0600); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	data, err := os.ReadFile(publisher.cachePath)
	if err != nil {
		return false, err
	}
	var cached cachedHWMonInventory
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cached); err != nil {
		return false, fmt.Errorf("decode %s: %w", publisher.cachePath, err)
	}
	// Decoder keeps its position after the first JSON value. A second decode must
	// therefore reach EOF; otherwise the cache contains another value or trailing
	// non-whitespace data that the first decode would silently leave unread.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return false, fmt.Errorf("decode %s: unexpected data after inventory", publisher.cachePath)
	}
	if cached.Version != 1 {
		return false, fmt.Errorf("unsupported cache version %d", cached.Version)
	}
	if cached.Disks != nil {
		readings := readingsFromCache(cached.Disks.Readings)
		if _, err := publishHWMonFamily(device, "disk", &publisher.disks, readings, false); err != nil {
			return false, fmt.Errorf("restore disks: %w", err)
		}
	}
	if cached.HBAs != nil {
		readings := readingsFromCache(cached.HBAs.Readings)
		if _, err := publishHWMonFamily(device, "hba", &publisher.hbas, readings, true); err != nil {
			return false, fmt.Errorf("restore HBA: %w", err)
		}
	}
	log.Printf("restored cached hwmon inventory from %s", publisher.cachePath)
	return cached.Disks != nil || cached.HBAs != nil, nil
}

func (publisher *hwmonPublisher) saveCache() error {
	cached := cachedHWMonInventory{Version: 1}
	if publisher.disks.initialized {
		cached.Disks = &cachedHWMonFamily{Readings: readingsToCache(publisher.disks.readings)}
	}
	if publisher.hbas.initialized {
		cached.HBAs = &cachedHWMonFamily{Readings: readingsToCache(publisher.hbas.readings)}
	}
	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(publisher.cachePath), 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(publisher.cachePath), ".hwmon-inventory-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, publisher.cachePath)
}

func readingsToCache(readings []hwmonReading) []cachedHWMonReading {
	cached := make([]cachedHWMonReading, 0, len(readings))
	for _, reading := range readings {
		cached = append(cached, cachedHWMonReading{
			ID: reading.id, Label: reading.label, Members: append([]string(nil), reading.members...),
		})
	}
	return cached
}

func readingsFromCache(cached []cachedHWMonReading) []hwmonReading {
	readings := make([]hwmonReading, 0, len(cached))
	for _, reading := range cached {
		readings = append(readings, hwmonReading{
			id: reading.ID, label: reading.Label, temperature: hwmonFailsafeTemp,
			members: append([]string(nil), reading.Members...),
		})
	}
	return readings
}

func parseRestartUnits(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var units []string
	seen := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		unit := strings.TrimSpace(item)
		if unit == "" || strings.HasPrefix(unit, "-") || strings.ContainsAny(unit, " \t\r\n/") {
			return nil, fmt.Errorf("invalid systemd unit %q", unit)
		}
		if unit == "unraid-vsock-hwmon.service" {
			return nil, errors.New("unraid-vsock-hwmon.service cannot restart itself")
		}
		if _, duplicate := seen[unit]; duplicate {
			continue
		}
		seen[unit] = struct{}{}
		units = append(units, unit)
	}
	return units, nil
}

func restartSystemdUnits(ctx context.Context, units []string) error {
	if len(units) == 0 {
		return nil
	}
	args := append([]string{"try-restart", "--no-block", "--"}, units...)
	output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart topology consumers: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeHWMonReadings(path, namespace, operation string, readings []hwmonReading) (err error) {
	device, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, device.Close())
	}()
	return encodeHWMonReadings(device, namespace, operation, readings)
}

func encodeHWMonReadings(out io.Writer, namespace, operation string, readings []hwmonReading) error {
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	if operation != "configure" && operation != "commit" {
		return fmt.Errorf("invalid hwmon operation %q", operation)
	}
	for _, reading := range readings {
		if !strings.HasPrefix(reading.id, prefix) || len(reading.id) > maxHWMonIDSize ||
			strings.ContainsAny(reading.id, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor ID %q", reading.id)
		}
		if _, duplicate := ids[reading.id]; duplicate {
			return fmt.Errorf("duplicate hwmon sensor ID %q", reading.id)
		}
		ids[reading.id] = struct{}{}
		if reading.label == "" || len(reading.label) > maxHWMonLabelSize ||
			strings.ContainsAny(reading.label, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor label %q", reading.label)
		}
		if math.IsNaN(reading.temperature) || math.IsInf(reading.temperature, 0) {
			return fmt.Errorf("invalid temperature for %q", reading.id)
		}
		if reading.temperature < 0 || reading.temperature > 150 {
			return fmt.Errorf("temperature out of range for %q", reading.id)
		}
	}
	for _, reading := range readings {
		milliCelsius := int64(math.Round(reading.temperature * 1000))
		if _, err := fmt.Fprintf(out, "%s\t%d\t%s\n", reading.id, milliCelsius, reading.label); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s\t%s\n", operation, namespace)
	return err
}
