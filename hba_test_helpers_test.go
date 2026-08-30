package main

import "time"

func newHBACollector(interval time.Duration, mode hbaMode) *hbaCollector {
	return newConfiguredHBACollector(interval, mode, hbaBackendMPT3CTL)
}
