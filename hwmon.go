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
		for _, disk := range internalDisks {
			if disk.Kind() == group.kind {
				groupDisks = append(groupDisks, disk)
			}
		}
		if len(groupDisks) < minHWMonGroupSize {
			continue
		}
		diskReadings = append(diskReadings, hwmonReading{
			id:    "disk:group:" + string(group.kind),
			label: group.label,
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

	log.Printf("publishing dynamic Unraid temperatures through %s every %s", *device, *interval)
	failed := false
	for {
		err := publishHWMonState(ctx, uint32(*cid), uint32(*port), *device, sensors.Fetch)
		if err != nil && !failed {
			log.Printf("hwmon update failed; existing sensors will apply their failsafe: %v", err)
			failed = true
		} else if err == nil && failed {
			log.Printf("hwmon updates recovered")
			failed = false
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

func publishHWMonState(
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
	// A committed snapshot is authoritative: any omitted sensor is removed by
	// virt-temp. Never commit a family whose collection failed; leave its
	// existing devices untouched so their watchdog can apply the failsafe.
	if state.Error != "" {
		diskErr = fmt.Errorf("disks: %s", state.Error)
	} else if err := writeHWMonReadings(device, "disk", disks); err != nil {
		diskErr = fmt.Errorf("disks: %w", err)
	}
	if state.HBAError != "" {
		hbaErr = fmt.Errorf("HBA: %s", state.HBAError)
	} else if err := writeHWMonReadings(device, "hba", hbas); err != nil {
		hbaErr = fmt.Errorf("HBA: %w", err)
	}
	return errors.Join(diskErr, hbaErr)
}

func writeHWMonReadings(path, namespace string, readings []hwmonReading) (err error) {
	device, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, device.Close())
	}()
	return encodeHWMonReadings(device, namespace, readings)
}

func encodeHWMonReadings(out io.Writer, namespace string, readings []hwmonReading) error {
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return fmt.Errorf("invalid hwmon namespace %q", namespace)
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
	_, err := fmt.Fprintf(out, "commit\t%s\n", namespace)
	return err
}
