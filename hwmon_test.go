// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRestartUnits(t *testing.T) {
	units, err := parseRestartUnits("coolercontrold.service, fan2go.service,coolercontrold.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"coolercontrold.service", "fan2go.service"}; !reflect.DeepEqual(units, want) {
		t.Fatalf("units = %#v, want %#v", units, want)
	}
	for _, value := range []string{
		"--no-block",
		"coolercontrold*",
		"fan?go.service",
		"[cf]an.service",
	} {
		t.Run("reject "+value, func(t *testing.T) {
			if _, err := parseRestartUnits(value); err == nil {
				t.Fatalf("invalid unit %q accepted", value)
			}
		})
	}
	for _, value := range []string{
		"unraid-vsock-hwmon",
		"unraid-vsock-hwmon.service",
	} {
		t.Run("reject self "+value, func(t *testing.T) {
			if _, err := parseRestartUnits(value); err == nil || !strings.Contains(err.Error(), "cannot restart itself") {
				t.Fatalf("self-restart unit %q returned %v", value, err)
			}
		})
	}
}

func TestShouldRestartConsumers(t *testing.T) {
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
			got := publisher.shouldRestartConsumers(test.reconfigured, test.firstGuestSnapshot)
			if got != test.want {
				t.Fatalf("shouldRestartConsumers() = %v, want %v", got, test.want)
			}
		})
	}
}
