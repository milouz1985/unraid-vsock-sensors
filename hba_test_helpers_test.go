// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func newTestHBACollector(interval time.Duration, mode hbaMode) *hbaCollector {
	return newConfiguredHBACollector(interval, mode, hbaBackendMPT3CTL)
}

type hbaSnapshotReaderFunc func(context.Context) ([]sensors.HBA, error)

func (read hbaSnapshotReaderFunc) collect(ctx context.Context) ([]sensors.HBA, error) {
	return read(ctx)
}
