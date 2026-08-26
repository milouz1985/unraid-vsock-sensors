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
	interval time.Duration
	mu       sync.RWMutex
	readings []sensors.HBA
	err      error
	// collect is replaceable in tests to simulate a slow StorCLI command.
	collect func(context.Context) ([]sensors.HBA, error)
}

const storcliTimeout = 10 * time.Second

func newHBACollector(interval time.Duration) *hbaCollector {
	return &hbaCollector{
		interval: interval,
		err:      errors.New("HBA temperatures have not been collected yet"),
		collect:  collectHBAs,
	}
}

// `(c *hbaCollector)` is the method receiver: it means that run belongs to the
// hbaCollector type. Inside the method, `c` refers to the specific collector on
// which `hbas.run(ctx)` was called. The asterisk means that the receiver is a
// pointer, so the method works with the collector's actual shared state
// (readings, error, and mutex) rather than with a copy.
//
// run owns the refresh loop. It is the only goroutine that calls StorCLI.
func (c *hbaCollector) run(ctx context.Context) {
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

	// Do not hold the lock here: StorCLI may take up to ten seconds, while
	// incoming vsock requests must remain able to read the current snapshot.
	readings, err := c.collect(ctx)

	// Publishing the values and their error under the same short lock prevents
	// readers from observing parts of two different refreshes.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		// Do not publish a stale temperature. Downstream consumers such as
		// CoolerControl apply their own missing-reading policy.
		c.readings = nil
		return
	}
	c.readings = readings
}

// read never invokes StorCLI. The copy keeps callers from modifying the slice
// shared by the collector and other requests.
func (c *hbaCollector) read() ([]sensors.HBA, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Return another clone so callers cannot modify the collector's snapshot.
	return slices.Clone(c.readings), c.err
}

func collectHBAs(ctx context.Context) ([]sensors.HBA, error) {
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
	if err == nil && len(readings) == 0 {
		err = errors.New("storcli returned no ROC temperature sensor")
	}
	return readings, err
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
		if selector == "hba" || sensor.Name == selector {
			result = append(result, sensor)
		}
	}
	return result
}
