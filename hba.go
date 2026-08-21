package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hba struct {
	Name string  `json:"name"`
	Temp float64 `json:"temp_c"`
}

type hbaCollector struct {
	maxAge   time.Duration
	mu       sync.Mutex
	readAt   time.Time
	readings []hba
	err      error
}

// read returns the cached HBA temperatures when they are still recent.
// Otherwise, it runs StorCLI once and refreshes the cache.
func (c *hbaCollector) read() ([]hba, error) {
	// Only one request may read or refresh the shared cache at a time.
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.readAt.IsZero() && time.Since(c.readAt) < c.maxAge {
		// Return a copy so callers cannot modify the collector's cached slice.
		return append([]hba(nil), c.readings...), c.err
	}

	c.readAt = time.Now()
	c.readings = nil

	// Stop StorCLI if it has not answered after ten seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
	if err == nil {
		c.readings, err = parseStorCLI(out)
		if err == nil && len(c.readings) == 0 {
			err = errors.New("storcli returned no ROC temperature sensor")
		}
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("storcli timeout: %w", ctx.Err())
	}

	c.err = err
	return append([]hba(nil), c.readings...), c.err
}

func parseStorCLI(data []byte) ([]hba, error) {
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

	var result []hba
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
			if err != nil {
				return nil, fmt.Errorf("storcli controller %d invalid temperature %q", id, property.Value)
			}
			result = append(result, hba{Name: fmt.Sprintf("hba%d", id), Temp: temp})
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

func selectHBAs(hbas []hba, selector string) []hba {
	selector = strings.ToLower(selector)
	var result []hba
	for _, sensor := range hbas {
		if selector == "hba" || strings.EqualFold(sensor.Name, selector) {
			result = append(result, sensor)
		}
	}
	return result
}
