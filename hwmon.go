package main

import (
	"bytes"
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
	"sort"
	"strings"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultHWMonInterval  = time.Second
	virtTempDevicePath    = "/dev/virt-temp"
	defaultHWMonCache     = "/var/lib/unraid-vsock-sensors/hwmon-inventory.json"
	hwmonFailsafeTemp     = 100.0
	minHWMonGroupSize     = 2
	maxHWMonIDSize        = 63
	maxHWMonLabelSize     = 95
	systemdRestartTimeout = 10 * time.Second
)

type hwmonSensor struct {
	id      string
	label   string
	members []string
}

type hwmonSample struct {
	sensor      hwmonSensor
	temperature float64
}

type hwmonInventory struct {
	initialized bool
	sensors     []hwmonSensor
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
	Sensors []cachedHWMonSensor `json:"readings"`
}

type cachedHWMonSensor struct {
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

func makeHWMonSamples(state sensors.Response) (diskSamples, hbaSamples []hwmonSample) {
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
		sort.Strings(members)
		maximum := sensors.MaxTemperature(groupDisks, func(disk sensors.Disk) float64 {
			return disk.Temp
		})
		for _, disk := range groupDisks {
			if disk.Unavailable {
				maximum = hwmonFailsafeTemp
				break
			}
		}
		diskSamples = append(diskSamples, hwmonSample{
			sensor: hwmonSensor{
				id: "disk:group:" + string(group.kind), label: group.label, members: members,
			},
			temperature: maximum,
		})
	}

	for _, disk := range internalDisks {
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
	}
	if restored {
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
	disks, hbas := makeHWMonSamples(state)
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
	current []hwmonSample,
	allowEmpty bool,
) (bool, error) {
	return publishHWMonFamilyWithWriter(path, namespace, inventory, current, allowEmpty, writeHWMonSamples)
}

func publishHWMonFamilyWithWriter(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonSample,
	allowEmpty bool,
	write func(string, string, string, []hwmonSample) error,
) (bool, error) {
	if len(current) == 0 && !allowEmpty {
		return false, errors.New("inventory is empty; waiting for sensors")
	}
	if !inventory.initialized {
		if err := write(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.initialized = true
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}
	if !sameHWMonTopology(inventory.sensors, current) {
		if err := write(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}

	currentByID := make(map[string]hwmonSample, len(current))
	for _, sample := range current {
		currentByID[sample.sensor.id] = sample
	}
	updates := make([]hwmonSample, 0, len(inventory.sensors))
	for _, expected := range inventory.sensors {
		reading, found := currentByID[expected.id]
		if !found {
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
			// such as /dev/sdX. Keep the configured label and use only the stable
			// ID to associate a new temperature.
			reading.sensor = expected
			updates = append(updates, reading)
		}
	}
	if err := write(path, namespace, "commit", updates); err != nil {
		if !errors.Is(err, syscall.ESTALE) {
			return false, err
		}
		if configureErr := write(path, namespace, "configure", current); configureErr != nil {
			return false, fmt.Errorf("reconfigure stale %s inventory: %w", namespace, configureErr)
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}
	return false, nil
}

func sameHWMonTopology(expected []hwmonSensor, current []hwmonSample) bool {
	if len(expected) != len(current) {
		return false
	}
	currentByID := make(map[string]hwmonSensor, len(current))
	for _, sample := range current {
		currentByID[sample.sensor.id] = sample.sensor
	}
	for _, reading := range expected {
		other, found := currentByID[reading.id]
		if !found || !slices.Equal(reading.members, other.members) {
			return false
		}
	}
	return true
}

func sensorsFromSamples(samples []hwmonSample) []hwmonSensor {
	sensors := make([]hwmonSensor, 0, len(samples))
	for _, sample := range samples {
		sensor := sample.sensor
		sensor.members = append([]string(nil), sensor.members...)
		sensors = append(sensors, sensor)
	}
	return sensors
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
	decoder := json.NewDecoder(bytes.NewReader(data))
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
	reconfigured := false
	if cached.Disks != nil {
		readings := samplesFromCache(cached.Disks.Sensors)
		changed, err := publishHWMonFamily(device, "disk", &publisher.disks, readings, false)
		if err != nil {
			return reconfigured, fmt.Errorf("restore disks: %w", err)
		}
		reconfigured = reconfigured || changed
	}
	if cached.HBAs != nil {
		readings := samplesFromCache(cached.HBAs.Sensors)
		changed, err := publishHWMonFamily(device, "hba", &publisher.hbas, readings, true)
		if err != nil {
			return reconfigured, fmt.Errorf("restore HBA: %w", err)
		}
		reconfigured = reconfigured || changed
	}
	log.Printf("restored cached hwmon inventory from %s", publisher.cachePath)
	return reconfigured, nil
}

func (publisher *hwmonPublisher) saveCache() error {
	cached := cachedHWMonInventory{Version: 1}
	if publisher.disks.initialized {
		cached.Disks = &cachedHWMonFamily{Sensors: sensorsToCache(publisher.disks.sensors)}
	}
	if publisher.hbas.initialized {
		cached.HBAs = &cachedHWMonFamily{Sensors: sensorsToCache(publisher.hbas.sensors)}
	}
	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directoryPath := filepath.Dir(publisher.cachePath)
	if err := os.MkdirAll(directoryPath, 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directoryPath, ".hwmon-inventory-*")
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
	if err := os.Rename(temporaryPath, publisher.cachePath); err != nil {
		return err
	}
	// Syncing the file makes its contents durable; syncing the directory after
	// the rename also makes the new directory entry durable across a power loss.
	return syncDirectory(directoryPath)
}

func syncDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	return directory.Sync()
}

func sensorsToCache(sensors []hwmonSensor) []cachedHWMonSensor {
	cached := make([]cachedHWMonSensor, 0, len(sensors))
	for _, sensor := range sensors {
		cached = append(cached, cachedHWMonSensor{
			ID: sensor.id, Label: sensor.label, Members: append([]string(nil), sensor.members...),
		})
	}
	return cached
}

func samplesFromCache(cached []cachedHWMonSensor) []hwmonSample {
	readings := make([]hwmonSample, 0, len(cached))
	for _, reading := range cached {
		readings = append(readings, hwmonSample{
			sensor: hwmonSensor{
				id: reading.ID, label: reading.Label, members: append([]string(nil), reading.Members...),
			},
			temperature: hwmonFailsafeTemp,
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
	ctx, cancel := context.WithTimeout(ctx, systemdRestartTimeout)
	defer cancel()
	args := append([]string{"try-restart", "--no-block", "--"}, units...)
	output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart topology consumers: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeHWMonSamples(path, namespace, operation string, readings []hwmonSample) (err error) {
	device, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, device.Close())
	}()
	return encodeHWMonSamples(device, namespace, operation, readings)
}

// encodeHWMonSamples writes the text protocol consumed by virt-temp's
// device_write(). Each sample is one tab-separated write:
//
//	sample\t<stable ID>\t<temperature in milli°C>\t<label>\n
//
// The final write atomically applies the samples collected on this open file:
//
//	configure\t<namespace>\n
//	commit\t<namespace>\n
//
// IDs and labels cannot contain tabs or newlines, so the kernel parser does
// not need quoting or escaping rules.
func encodeHWMonSamples(out io.Writer, namespace, operation string, readings []hwmonSample) error {
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	if operation != "configure" && operation != "commit" {
		return fmt.Errorf("invalid hwmon operation %q", operation)
	}
	for _, reading := range readings {
		sensor := reading.sensor
		if !strings.HasPrefix(sensor.id, prefix) || len(sensor.id) > maxHWMonIDSize ||
			strings.ContainsAny(sensor.id, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor ID %q", sensor.id)
		}
		if _, duplicate := ids[sensor.id]; duplicate {
			return fmt.Errorf("duplicate hwmon sensor ID %q", sensor.id)
		}
		ids[sensor.id] = struct{}{}
		if sensor.label == "" || len(sensor.label) > maxHWMonLabelSize ||
			strings.ContainsAny(sensor.label, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor label %q", sensor.label)
		}
		if math.IsNaN(reading.temperature) || math.IsInf(reading.temperature, 0) {
			return fmt.Errorf("invalid temperature for %q", sensor.id)
		}
		if reading.temperature < 0 || reading.temperature > 150 {
			return fmt.Errorf("temperature out of range for %q", sensor.id)
		}
	}
	for _, reading := range readings {
		milliCelsius := int64(math.Round(reading.temperature * 1000))
		if _, err := fmt.Fprintf(out, "sample\t%s\t%d\t%s\n", reading.sensor.id, milliCelsius, reading.sensor.label); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s\t%s\n", operation, namespace)
	return err
}
