// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func newTestHBACollector(interval time.Duration, mode hbaMode) *hbaCollector {
	collector, err := newConfiguredHBACollector(mode, hbaBackendMPT3CTL)
	if err != nil {
		panic(err)
	}
	collector.interval = interval
	return collector
}

type hbaSnapshotReaderFunc func(context.Context) ([]sensors.HBA, error)

func (read hbaSnapshotReaderFunc) collect(ctx context.Context) ([]sensors.HBA, error) {
	return read(ctx)
}
