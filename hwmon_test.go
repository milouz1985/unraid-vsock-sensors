// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShouldNotifyTopologyChanged(t *testing.T) {
	tests := []struct {
		name               string
		reconfigured       bool
		firstGuestSnapshot bool
		familyInitialized  bool
		diskPending        bool
		hbaPending         bool
		want               bool
	}{
		{
			name:              "reconfigured",
			reconfigured:      true,
			familyInitialized: true,
			want:              true,
		},
		{
			name:              "reconfigured with disk reconciliation pending",
			reconfigured:      true,
			familyInitialized: true,
			diskPending:       true,
		},
		{
			name:               "first snapshot with initialized family",
			firstGuestSnapshot: true,
			familyInitialized:  true,
			want:               true,
		},
		{
			name:               "first snapshot with HBA reconciliation pending",
			firstGuestSnapshot: true,
			familyInitialized:  true,
			hbaPending:         true,
		},
		{
			name:               "first snapshot without initialized family",
			firstGuestSnapshot: true,
		},
		{
			name:              "subsequent unchanged snapshot",
			familyInitialized: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publisher := &hwmonPublisher{
				disks: hwmonInventory{needsReconcile: test.diskPending},
				hbas:  hwmonInventory{needsReconcile: test.hbaPending},
			}
			if test.familyInitialized {
				// A known, authoritatively empty family is initialized; nil is not.
				publisher.disks.sensors = []hwmonSensor{}
			}
			got := publisher.shouldNotifyTopologyChanged(test.reconfigured, test.firstGuestSnapshot)
			if got != test.want {
				t.Fatalf("shouldNotifyTopologyChanged() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNotifyTopologyChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology-changed")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := notifyTopologyChanged(path); err != nil {
			t.Fatalf("notification %d: %v", attempt, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat notification %d: %v", attempt, err)
		}
		if info.Size() != 0 {
			t.Fatalf("notification %d size = %d, want an empty event file", attempt, info.Size())
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("notification %d mode = %o, want 600", attempt, got)
		}
	}
}
