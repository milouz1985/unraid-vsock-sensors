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
