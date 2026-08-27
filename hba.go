package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type hbaCollector struct {
	interval    time.Duration
	mode        hbaMode
	mu          sync.RWMutex
	readings    []sensors.HBA
	err         error
	hadReadings bool
	// collect is replaceable in tests to simulate a slow StorCLI command.
	collect func(context.Context) ([]sensors.HBA, error)
}

type hbaMetadata struct {
	id         string
	model      string
	pciAddress string
}

type hbaReader struct {
	metadata map[int]hbaMetadata
	discover func(context.Context) (map[int]hbaMetadata, error)
	read     func(context.Context) ([]sensors.HBA, error)
}

func newHBAReader() *hbaReader {
	return &hbaReader{discover: discoverHBAs, read: readHBATemperatures}
}

func (r *hbaReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	if r.metadata == nil {
		metadata, err := r.discover(ctx)
		if err != nil {
			return nil, err
		}
		r.metadata = metadata
	}
	readings, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	if !applyHBAMetadata(readings, r.metadata) {
		return nil, errors.New("storcli temperature references an unknown controller; restart the service to refresh HBA metadata")
	}
	return readings, nil
}

func applyHBAMetadata(readings []sensors.HBA, metadata map[int]hbaMetadata) bool {
	for index := range readings {
		controller, err := hbaControllerNumber(readings[index].Name)
		if err != nil {
			return false
		}
		identity, ok := metadata[controller]
		if !ok {
			return false
		}
		readings[index].ID = identity.id
		readings[index].Model = identity.model
		readings[index].PCIAddress = identity.pciAddress
	}
	return true
}

func hbaControllerNumber(name string) (int, error) {
	if !strings.HasPrefix(name, "hba") {
		return 0, fmt.Errorf("invalid HBA name %q", name)
	}
	return strconv.Atoi(strings.TrimPrefix(name, "hba"))
}

// StorCLI normally completes in about 1.5 seconds. Five seconds leaves enough
// margin under load while limiting how long stale readings survive a hung call.
const storcliTimeout = 5 * time.Second

type hbaMode string

const (
	hbaModeAuto     hbaMode = "auto"
	hbaModeEnabled  hbaMode = "enabled"
	hbaModeDisabled hbaMode = "disabled"
)

var errNoHBA = errors.New("storcli returned no controllers")

func newHBACollector(interval time.Duration, mode hbaMode) *hbaCollector {
	reader := newHBAReader()
	collector := &hbaCollector{
		interval: interval,
		mode:     mode,
		err:      errors.New("HBA temperatures have not been collected yet"),
		collect:  reader.collect,
	}
	if mode == hbaModeDisabled {
		collector.err = nil
	}
	return collector
}

// `(c *hbaCollector)` is the method receiver: it means that run belongs to the
// hbaCollector type. Inside the method, `c` refers to the specific collector on
// which `hbas.run(ctx)` was called. The asterisk means that the receiver is a
// pointer, so the method works with the collector's actual shared state
// (readings, error, and mutex) rather than with a copy.
//
// run owns the refresh loop. It is the only goroutine that calls StorCLI.
func (c *hbaCollector) run(ctx context.Context) {
	if c.mode == hbaModeDisabled {
		return
	}
	// Collect immediately so the first value is available as soon as possible.
	c.refresh(ctx)
	// Use a timer rather than a ticker so the interval starts after each refresh.
	// A ticker could queue a tick while StorCLI is slow and trigger another call
	// immediately after the first one completes.
	timer := time.NewTimer(c.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.refresh(ctx)
			timer.Reset(c.interval)
		}
	}
}

func (c *hbaCollector) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, storcliTimeout)
	defer cancel()

	// Do not hold the lock here: StorCLI may take up to five seconds, while
	// incoming vsock requests must remain able to read the current snapshot.
	readings, err := c.collect(ctx)

	// Publishing the values and their error under the same short lock prevents
	// readers from observing parts of two different refreshes.
	c.mu.Lock()
	defer c.mu.Unlock()
	// In auto mode, an initially absent StorCLI executable or controller means
	// that this system has no HBA monitoring to expose. Once an HBA has been
	// detected, the same condition is a collection failure: keeping the error
	// prevents hwmon clients from deleting existing sensors instead of letting
	// their watchdog apply its failsafe.
	absent := errors.Is(err, exec.ErrNotFound) || errors.Is(err, errNoHBA)
	if c.mode == hbaModeAuto && !c.hadReadings && absent {
		err = nil
	}
	c.err = err
	if err != nil {
		// Do not publish a stale temperature. Downstream consumers such as
		// CoolerControl apply their own missing-reading policy.
		c.readings = nil
		return
	}
	c.readings = readings
	if len(readings) > 0 {
		c.hadReadings = true
	}
}

// read never invokes StorCLI. The copy keeps callers from modifying the slice
// shared by the collector and other requests.
func (c *hbaCollector) read() ([]sensors.HBA, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Return another clone so callers cannot modify the collector's snapshot.
	return slices.Clone(c.readings), c.err
}

func readHBATemperatures(ctx context.Context) ([]sensors.HBA, error) {
	command := exec.CommandContext(
		ctx,
		"storcli",
		"/cALL",
		"show",
		"temperature",
		"J",
		"nolog",
	)
	out, err := command.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("storcli timeout: %w", ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	readings, err := parseStorCLI(out)
	return readings, err
}

func discoverHBAs(ctx context.Context) (map[int]hbaMetadata, error) {
	command := exec.CommandContext(ctx, "storcli", "/cALL", "show", "J", "nolog")
	out, err := command.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("storcli discovery timeout: %w", ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	return parseStorCLIMetadata(out)
}

func parseStorCLIMetadata(data []byte) (map[int]hbaMetadata, error) {
	type basics struct {
		Model        string `json:"Model"`
		ProductName  string `json:"Product Name"`
		SerialNumber string `json:"Serial Number"`
		SASAddress   string `json:"SAS Address"`
		PCIAddress   string `json:"PCI Address"`
	}
	var root struct {
		Controllers []struct {
			CommandStatus struct {
				Controller int    `json:"Controller"`
				Status     string `json:"Status"`
			} `json:"Command Status"`
			ResponseData struct {
				Basics     basics `json:"Basics"`
				Model      string `json:"Model"`
				Product    string `json:"Product Name"`
				Serial     string `json:"Serial Number"`
				SASAddress string `json:"SAS Address"`
				PCIAddress string `json:"PCI Address"`
			} `json:"Response Data"`
		} `json:"Controllers"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse storcli discovery JSON: %w", err)
	}
	if len(root.Controllers) == 0 {
		return nil, errNoHBA
	}

	result := make(map[int]hbaMetadata, len(root.Controllers))
	ids := make(map[string]int, len(root.Controllers))
	for _, controller := range root.Controllers {
		number := controller.CommandStatus.Controller
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", number, controller.CommandStatus.Status)
		}
		data := controller.ResponseData
		serial := firstHBAValue(data.Basics.SerialNumber, data.Serial)
		sasAddress := firstHBAValue(data.Basics.SASAddress, data.SASAddress)
		pciAddress := normalizePCIAddress(firstHBAValue(data.Basics.PCIAddress, data.PCIAddress))
		model := firstHBAValue(data.Basics.Model, data.Basics.ProductName, data.Model, data.Product)
		id := hbaStableID(number, serial, sasAddress, pciAddress)
		if previous, duplicate := ids[id]; duplicate {
			return nil, fmt.Errorf("storcli controllers %d and %d have duplicate identity %q", previous, number, id)
		}
		ids[id] = number
		result[number] = hbaMetadata{id: id, model: model, pciAddress: pciAddress}
	}
	return result, nil
}

func firstHBAValue(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		switch strings.ToLower(value) {
		case "", "n/a", "na", "none", "unknown":
			continue
		default:
			return value
		}
	}
	return ""
}

func hbaStableID(controller int, serial, sasAddress, pciAddress string) string {
	if serial = firstHBAValue(serial); serial != "" {
		return "serial:" + strings.ToLower(serial)
	}
	if sasAddress = firstHBAValue(sasAddress); sasAddress != "" {
		return "sas:" + strings.TrimPrefix(strings.ToLower(sasAddress), "0x")
	}
	if pciAddress != "" {
		return "pci:" + pciAddress
	}
	return fmt.Sprintf("controller:%d", controller)
}

func normalizePCIAddress(address string) string {
	parts := strings.Split(strings.TrimSpace(address), ":")
	if len(parts) != 4 {
		return ""
	}
	values := make([]uint64, len(parts))
	for index, part := range parts {
		value, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return ""
		}
		values[index] = value
	}
	if values[0] > 0xffff || values[1] > 0xff || values[2] > 0x1f || values[3] > 7 {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", values[0], values[1], values[2], values[3])
}

func parseStorCLI(data []byte) ([]sensors.HBA, error) {
	var root struct {
		Controllers []struct {
			CommandStatus struct {
				Controller int    `json:"Controller"`
				Status     string `json:"Status"`
			} `json:"Command Status"`
			ResponseData struct {
				ControllerProperties []struct {
					Property string `json:"Ctrl_Prop"`
					Value    string `json:"Value"`
				} `json:"Controller Properties"`
			} `json:"Response Data"`
		} `json:"Controllers"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse storcli JSON: %w", err)
	}
	if len(root.Controllers) == 0 {
		return nil, errNoHBA
	}

	var result []sensors.HBA
	for _, controller := range root.Controllers {
		id := controller.CommandStatus.Controller
		if controller.CommandStatus.Status != "Success" {
			return nil, fmt.Errorf("storcli controller %d status is %q", id, controller.CommandStatus.Status)
		}

		found := false
		for _, property := range controller.ResponseData.ControllerProperties {
			if property.Property != "ROC temperature(Degree Celsius)" {
				continue
			}
			temp, err := strconv.ParseFloat(property.Value, 64)
			if err != nil || math.IsNaN(temp) || math.IsInf(temp, 0) {
				return nil, fmt.Errorf("storcli controller %d invalid temperature %q", id, property.Value)
			}
			result = append(result, sensors.HBA{Name: fmt.Sprintf("hba%d", id), Temp: temp})
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("storcli controller %d has no ROC temperature", id)
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func selectHBAs(hbas []sensors.HBA, selector string) []sensors.HBA {
	selector = strings.ToLower(selector)
	var result []sensors.HBA
	for _, sensor := range hbas {
		if selector == "all" || sensor.Name == selector {
			result = append(result, sensor)
		}
	}
	return result
}
