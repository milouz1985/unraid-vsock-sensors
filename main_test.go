package main

import (
	"testing"
	"time"
)

func TestServerResponseMetadata(t *testing.T) {
	response := collectorSnapshot(
		newDiskCollector("unused", time.Minute),
		newHBACollector(time.Minute, hbaModeDisabled),
	)

	if response.Version != version {
		t.Fatalf("got version %q, want %q", response.Version, version)
	}
	if !response.HBADisabled {
		t.Fatal("disabled HBA collection was not reported")
	}
	if response.Error == "" {
		t.Fatal("uncollected disks were reported as available")
	}
}
