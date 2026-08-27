package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"unraid-vsock-sensors/internal/sensors"
	"unraid-vsock-sensors/internal/vsockaddr"
)

const (
	defaultHWMonInterval = time.Second
	virtTempDevicePath   = "/dev/virt-temp"
	minHWMonGroupSize    = 2
	maxHWMonIDSize       = 63
	maxHWMonLabelSize    = 95
)

type hwmonReading struct {
	id          string
	label       string
	temperature float64
	members     []string
}

type hwmonInventory struct {
	initialized bool
	readings    []hwmonReading
}

type hwmonPublisher struct {
	disks hwmonInventory
	hbas  hwmonInventory
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
		diskReadings = append(diskReadings, hwmonReading{
			id:      "disk:group:" + string(group.kind),
			label:   group.label,
			members: members,
			temperature: sensors.MaxTemperature(groupDisks, func(disk sensors.Disk) float64 {
				return disk.Temp
			}),
		})
	}

	for _, disk := range internalDisks {
		diskReadings = append(diskReadings, hwmonReading{
			id:          "disk:" + disk.ID,
			label:       fmt.Sprintf("%s (%s)", disk.Name, disk.Device),
			temperature: disk.Temp,
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("publishing fixed Unraid hwmon inventories through %s every %s", *device, *interval)
	publisher := &hwmonPublisher{}
	lastError := ""
	for {
		err := publisher.publish(ctx, uint32(*cid), uint32(*port), *device, sensors.Fetch)
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
) error {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	state, err := fetch(ctx, cid, port)
	if err != nil {
		return err
	}
	disks, hbas := makeHWMonReadings(state)
	var diskErr, hbaErr error
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if err := publishHWMonFamily(device, "disk", &publisher.disks, disks, false); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if err := publishHWMonFamily(device, "hba", &publisher.hbas, hbas, state.HBADisabled); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	}
	return errors.Join(diskErr, hbaErr)
}

func publishHWMonFamily(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonReading,
	allowEmpty bool,
) error {
	if !inventory.initialized {
		if len(current) == 0 && !allowEmpty {
			return errors.New("initial inventory is empty; waiting for sensors")
		}
		if err := writeHWMonReadings(path, namespace, "configure", current); err != nil {
			return err
		}
		inventory.initialized = true
		inventory.readings = append([]hwmonReading(nil), current...)
		return nil
	}

	currentByID := make(map[string]hwmonReading, len(current))
	for _, reading := range current {
		currentByID[reading.id] = reading
	}
	expectedByID := make(map[string]hwmonReading, len(inventory.readings))
	for _, reading := range inventory.readings {
		expectedByID[reading.id] = reading
	}

	updates := make([]hwmonReading, 0, len(inventory.readings))
	var topologyErrors []error
	for _, expected := range inventory.readings {
		reading, found := currentByID[expected.id]
		if !found {
			topologyErrors = append(topologyErrors, fmt.Errorf(
				"expected sensor %q is missing; failsafe active, restart unraid-vsock-hwmon.service if intentional",
				expected.label,
			))
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
	for _, reading := range current {
		if _, expected := expectedByID[reading.id]; !expected {
			topologyErrors = append(topologyErrors, fmt.Errorf(
				"new sensor %q is not published; restart unraid-vsock-hwmon.service to accept the new topology",
				reading.label,
			))
		}
	}
	if err := writeHWMonReadings(path, namespace, "commit", updates); err != nil {
		return errors.Join(append(topologyErrors, err)...)
	}
	return errors.Join(topologyErrors...)
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
