// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestReadStorCLITemperaturesLocksCommand locks the exact argv and controller
// targeting of readStorCLITemperatures. It installs a fake storcli executable
// that records its arguments and returns a controlled temperature fixture,
// then verifies the command was invoked with the expected arguments and that
// the output was parsed correctly.
func TestReadStorCLITemperaturesLocksCommand(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "storcli-args.txt")
	fixture := `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"},{"Ctrl_Prop":"Ctrl temperature(Degree Celsius)","Value":"45"}]}}]}`
	// Use a bash script to reliably capture all arguments, including the first.
	body := "for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> '" + argvPath + "'; done\nprintf '" + fixture + "'"

	installTestStorCLIBash(t, body)

	readings, err := readStorCLITemperatures(context.Background())
	if err != nil {
		t.Fatalf("readStorCLITemperatures: %v", err)
	}

	// Verify the output was parsed.
	if len(readings) != 1 {
		t.Fatalf("readings = %#v, want 1 controller", readings)
	}
	temps, ok := readings[0]
	if !ok {
		t.Fatal("controller 0 missing from readings")
	}
	if !equalOptionalTemperature(temps.ioc, float64Pointer(49)) {
		t.Fatalf("IOC temp = %v, want 49", temps.ioc)
	}
	if !equalOptionalTemperature(temps.board, float64Pointer(45)) {
		t.Fatalf("Board temp = %v, want 45", temps.board)
	}

	// Verify the exact argv (one argument per line).
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(data)), "\n")
	// The operation ("temperature") is used only for error messages, not as an
	// argument. The executable is invoked with the variadic args only.
	wantArgs := []string{"/cALL", "show", "temperature", "J", "nolog"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("argv = %v, want %v", gotArgs, wantArgs)
	}
}

// TestDiscoverStorCLIHBALocksCommand locks the exact argv of the StorCLI
// discovery command invoked by discoverStorCLIHBAs. It installs a fake storcli
// executable that records its arguments. This test only verifies the command
// line; the sysfs matching and the final discovery result are covered by the
// separate sysfs parsing tests.
func TestDiscoverStorCLIHBALocksCommand(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "storcli-args.txt")
	fixture := `{"Controllers":[{"Command Status":{"Controller":0,"Status":"Success"},"Response Data":{"Basics":{"Model":"SAS3008","SAS Address":"0x5000000000000001","PCI Address":"0000:06:10:0"}}}]}`
	body := "for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> '" + argvPath + "'; done\nprintf '" + fixture + "'"

	installTestStorCLIBash(t, body)

	// The discovery also reads sysfs (defaultSCSIHostRoot points to the real
	// /sys). It will fail on a system without a matching HBA, but the argv is
	// still recorded by the fake executable before the sysfs traversal.
	_, _ = discoverStorCLIHBAs(context.Background())

	// Verify the exact argv (one argument per line).
	data, readErr := os.ReadFile(argvPath)
	if readErr != nil {
		t.Fatalf("read recorded args: %v", readErr)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(data)), "\n")
	// The operation ("discovery") is used only for error messages, not as an
	// argument. The executable is invoked with the variadic args only.
	wantArgs := []string{"/cALL", "show", "J", "nolog"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("argv = %v, want %v", gotArgs, wantArgs)
	}
}

// installTestStorCLIBash installs a fake storcli executable using bash (instead
// of sh) to reliably capture all arguments including the first.
func installTestStorCLIBash(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "storcli")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
