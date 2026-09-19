// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

const (
	defaultPollAttributes     = 30 * time.Second
	maximumRecommendedPolling = 60 * time.Second
)

// pollAttributesState owns the effective poll_attributes interval and the
// de-duplicated log of the last value and configuration error it was read from.
type pollAttributesState struct {
	interval       time.Duration
	haveValid      bool
	logInitialized bool
	lastInterval   time.Duration
	lastError      string
}

func parsePollAttributes(data []byte) (time.Duration, error) {
	config, err := ini.Load(data)
	if err != nil {
		return 0, fmt.Errorf("parse var.ini: %w", err)
	}
	section := config.Section(ini.DefaultSection)
	key, err := section.GetKey("poll_attributes")
	if err != nil {
		return 0, errors.New("poll_attributes is missing")
	}
	raw := strings.TrimSpace(key.String())
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("poll_attributes %q is not a number", raw)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("poll_attributes %d is negative", seconds)
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("poll_attributes %d is too large", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

func readPollAttributes(path string) (time.Duration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultPollAttributes, fmt.Errorf("read %s: %w", path, err)
	}
	interval, err := parsePollAttributes(data)
	if err != nil {
		return defaultPollAttributes, fmt.Errorf("read %s: %w", path, err)
	}
	return interval, nil
}

// effective reads the current poll_attributes interval and resolves the
// effective value: a valid read replaces the last known good interval, while a
// failed read keeps the last valid interval when one was ever observed.
func (s *pollAttributesState) effective(varINIPath string) (time.Duration, error) {
	interval, err := readPollAttributes(varINIPath)
	if err == nil {
		s.interval = interval
		s.haveValid = true
		return interval, nil
	}
	if s.haveValid {
		return s.interval, err
	}
	return interval, err
}

func (s *pollAttributesState) logChange(interval time.Duration, configErr error) {
	errorMessage := errorText(configErr)
	if s.logInitialized && s.lastInterval == interval && s.lastError == errorMessage {
		return
	}
	s.logInitialized = true
	s.lastInterval = interval
	s.lastError = errorMessage
	logPollAttributes(interval, configErr)
}

func logPollAttributes(interval time.Duration, configErr error) {
	if configErr != nil {
		log.Printf("warning: %v; using %s for stalled-poll detection", configErr, interval)
		return
	}
	if interval == 0 {
		log.Printf("warning: Unraid automatic SMART polling is disabled (poll_attributes=0); disk temperatures cannot remain fresh automatically")
		return
	}
	log.Printf("Unraid SMART polling interval: %s", interval)
	if interval > maximumRecommendedPolling {
		log.Printf("warning: Unraid SMART polling interval is %s; fan control may react with several minutes of delay", interval)
	}
}
