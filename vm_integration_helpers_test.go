// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && integration

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireVMIntegrationTest(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("UVSS_VM_TEST") != "1" {
		t.Fatal("run through tests/vm/guest-tests.sh in a disposable VM")
	}
	for _, marker := range []string{"/etc/uvss-test-image", "/etc/uvss-test-image-version"} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("missing test image marker %s: %v", marker, err)
		}
	}
	output, err := exec.Command("systemd-detect-virt", "--vm").Output()
	virtualization := strings.TrimSpace(string(output))
	if err != nil || (virtualization != "kvm" && virtualization != "qemu") {
		t.Fatal("expected a QEMU/KVM guest")
	}
}

func runVMTestCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func testVirtTempModule(t *testing.T) string {
	t.Helper()
	module, err := filepath.Abs("virt-temp/module/virt-temp.ko")
	if err != nil {
		t.Fatal(err)
	}
	return module
}

func loadTestVirtTemp(t *testing.T) string {
	t.Helper()
	requireVMIntegrationTest(t)
	if _, err := os.Stat("/sys/module/virt_temp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("virt_temp already loaded or module state unreadable; refusing to modify it")
	}
	runVMTestCommand(t, "insmod", testVirtTempModule(t))
	t.Cleanup(func() {
		if _, err := os.Stat("/sys/module/virt_temp"); err == nil {
			if output, err := exec.Command("rmmod", "virt_temp").CombinedOutput(); err != nil {
				t.Errorf("unload virt_temp: %v\n%s", err, output)
			}
		}
	})
	if info, err := os.Stat(virtTempDevicePath); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("expected real character device: %v", err)
	}
	return virtTempDevicePath
}

func reloadTestVirtTemp(t *testing.T) {
	t.Helper()
	runVMTestCommand(t, "rmmod", "virt_temp")
	runVMTestCommand(t, "insmod", testVirtTempModule(t))
}

func vmHWMonPaths(t *testing.T, namespace, id string) []string {
	t.Helper()
	pattern := fmt.Sprintf("/sys/devices/platform/unraid_%s_%x/hwmon/hwmon*/temp1_input", namespace, id)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func readVMHWMonAttribute(t *testing.T, namespace, id, attribute string) string {
	t.Helper()
	matches := vmHWMonPaths(t, namespace, id)
	if len(matches) != 1 {
		t.Fatalf("expected one hwmon device for %s, got %v", id, matches)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(matches[0]), attribute))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func readVMHWMonTemp(t *testing.T, namespace, id string) string {
	t.Helper()
	return readVMHWMonAttribute(t, namespace, id, "temp1_input")
}

func requireVMHWMonTemp(t *testing.T, namespace, id, want string) {
	t.Helper()
	if got := readVMHWMonTemp(t, namespace, id); got != want {
		t.Fatalf("%s = %s milliCelsius, want %s", id, got, want)
	}
}
