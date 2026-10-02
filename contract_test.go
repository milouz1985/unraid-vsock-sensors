// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

// TestDiagnosticsContractFixture locks the Go -> PHP JSON contract for the
// diagnostics page. It generates the diagnostics snapshot JSON in memory from
// the production code, reads the versioned golden file
// (testdata/contract/diagnostics.json), and verifies that the two match. The
// golden file is the contract: a rename or type change on the Go side that
// breaks the PHP consumer makes this test fail. The golden is only rewritten
// when UPDATE_GOLDEN=1 is set; make check never sets it.
func TestDiagnosticsContractFixture(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-3600 * time.Second)

	// Build a representative diagnostics snapshot.
	service := serviceStatus{
		startedAt: startedAt,
		pid:       12345,
		publisher: publisherRuntimeStatus{
			status:          publisherStatusConnected,
			hostCID:         2,
			port:            990,
			lastConnectedAt: now.Add(-60 * time.Second),
			lastPublishedAt: now.Add(-5 * time.Second),
			lastError:       "",
		},
	}
	disks := diskCollectorStatus{
		updatedAt: now.Add(-10 * time.Second),
		err:       nil,
		disks: []diskRuntimeDisk{
			{
				disk:    unraidDisk{id: "serial001", name: "disk1", device: "sda", transport: "ata", rotational: true},
				reading: sensors.Disk{ID: "serial001", Name: "disk1", Device: "sda", Temp: 38.5, Unavailable: false},
				state:   diskState{thermalState: diskThermalValid, lastValidAt: now.Add(-10 * time.Second), lastSource: diskSourceEmhttpd},
			},
		},
		source: smartSourceStatus{
			source:        diskSourceEmhttpd,
			heartbeatSeen: true,
			lastHeartbeat: now.Add(-3 * time.Second),
			pollInterval:  30 * time.Second,
			initialized:   true,
		},
		stale: false,
	}
	hbas := hbaCollectorStatus{
		interval:         15 * time.Second,
		mode:             hbaModeEnabled,
		backend:          hbaBackendMPT3CTL,
		lastSuccessfulAt: now.Add(-10 * time.Second),
		lastSuccessfulSnapshot: []sensors.HBA{
			{ID: "sas:56c92bf0002e6705", Model: "LSI SAS3008", PCIAddress: "0000:06:00.0", Temp: 42, IOCTemp: float64Pointer(42), BoardTemp: float64Pointer(45)},
		},
		stale: false,
	}

	snapshot := buildDiagnosticsSnapshot(service, disks, hbas, now)

	goldenPath := filepath.Join("testdata", "contract", "diagnostics.json")
	verifyContractGolden(t, goldenPath, snapshot)
}

// TestDiskPolicyContractFixture locks the Go -> PHP JSON contract for the disk
// policy inventory. It generates the disk policy row JSON in memory from the
// production code, reads the versioned golden file
// (testdata/contract/disk-policy.json), and verifies that the two match. The
// golden is only rewritten when UPDATE_GOLDEN=1 is set; make check never sets
// it.
func TestDiskPolicyContractFixture(t *testing.T) {
	// The validation_error field is omitempty; set a non-empty value so the
	// fixture includes it. The PHP page reads it only when non-empty, but the
	// contract test verifies the field name is present in the JSON schema.
	row := diskPolicyRow{
		ID:              "serial001",
		Name:            "disk1",
		Device:          "sda",
		Transport:       "ata",
		Bus:             "pci",
		Policy:          diskPolicyInclude,
		Selected:        true,
		Eligible:        false,
		ValidationError: "disk is in an unknown state",
	}

	goldenPath := filepath.Join("testdata", "contract", "disk-policy.json")
	verifyContractGolden(t, goldenPath, row)
}

// verifyContractGolden generates the JSON for value in memory, compares it
// with the versioned golden file at goldenPath, and fails on mismatch. When
// UPDATE_GOLDEN=1 is set it rewrites the golden instead of failing.
func verifyContractGolden(t *testing.T, goldenPath string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, data, 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s", goldenPath)
		return
	}

	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to regenerate)", goldenPath, err)
	}

	var want, got any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatalf("decode golden %s: %v", goldenPath, err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode generated value: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("generated JSON does not match golden %s\n--- golden ---\n%s\n--- generated ---\n%s",
			goldenPath, golden, data)
	}
}
