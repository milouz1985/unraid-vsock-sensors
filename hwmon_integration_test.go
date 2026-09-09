//go:build linux && integration

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This test owns the module for its entire lifetime. It must never run on the
// Proxmox host that supplies production fan-control sensors.
func TestVMHWMon(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("UVSS_VM_TEST") != "1" {
		t.Fatal("run through tests/vm/guest-tests.sh in a disposable VM")
	}
	if _, err := os.Stat("/etc/uvss-test-image"); err != nil {
		t.Fatal("missing UVSS test image marker: ", err)
	}
	output, err := exec.Command("systemd-detect-virt", "--vm").Output()
	virtualization := strings.TrimSpace(string(output))
	if err != nil || (virtualization != "kvm" && virtualization != "qemu") {
		t.Fatal("expected a QEMU/KVM guest")
	}
	if _, err := os.Stat("/sys/module/virt_temp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("virt_temp already loaded or module state unreadable; refusing to modify it")
	}
	module, err := filepath.Abs("virt-temp/module/virt-temp.ko")
	if err != nil {
		t.Fatal(err)
	}
	command := func(name string, args ...string) {
		t.Helper()
		if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
	}
	command("insmod", module)
	t.Cleanup(func() {
		if _, err := os.Stat("/sys/module/virt_temp"); err == nil {
			if output, err := exec.Command("rmmod", "virt_temp").CombinedOutput(); err != nil {
				t.Errorf("unload virt_temp: %v\n%s", err, output)
			}
		}
	})
	device := "/dev/virt-temp"
	if info, err := os.Stat(device); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("expected real character device: %v", err)
	}
	paths := func(namespace, id string) []string {
		t.Helper()
		pattern := fmt.Sprintf("/sys/devices/platform/unraid_%s_%x/hwmon/hwmon*/temp1_input", namespace, id)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		return matches
	}
	read := func(namespace, id, attr string) string {
		t.Helper()
		matches := paths(namespace, id)
		if len(matches) != 1 {
			t.Fatalf("expected one hwmon device for %s, got %v", id, matches)
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(matches[0]), attr))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	wantTemp := func(namespace, id, want string) {
		t.Helper()
		if got := read(namespace, id, "temp1_input"); got != want {
			t.Fatalf("%s = %s milliCelsius, want %s", id, got, want)
		}
	}
	disks, hbas := hwmonInventory{}, hwmonInventory{}
	diskSamples := []hwmonSample{hwmonTestSample("disk:vm-a", "VM disk A", 34)}
	hbaSamples := []hwmonSample{hwmonTestSample("hba:vm-a", "VM HBA A", 48)}
	publish := func(namespace string, inventory *hwmonInventory, samples []hwmonSample, wantChanged bool) {
		t.Helper()
		changed, err := publishHWMonFamily(device, namespace, inventory, samples)
		if err != nil || changed != wantChanged {
			t.Fatalf("publish %s: changed=%v, want %v; err=%v", namespace, changed, wantChanged, err)
		}
	}
	t.Log("configure disk/HBA families and read real sysfs temperatures")
	publish("disk", &disks, diskSamples, true)
	publish("hba", &hbas, hbaSamples, true)
	wantTemp("disk", "disk:vm-a", "34000")
	wantTemp("hba", "hba:vm-a", "48000")
	if got := read("disk", "disk:vm-a", "name"); got != "unraid_vm_disk_a" {
		t.Fatalf("disk hwmon name = %q, want unraid_vm_disk_a", got)
	}
	if got := read("hba", "hba:vm-a", "name"); got != "unraid_vm_hba_a" {
		t.Fatalf("HBA hwmon name = %q, want unraid_vm_hba_a", got)
	}

	t.Log("commit preserves the configured label and device identity")
	initialPath := paths("disk", "disk:vm-a")[0]
	diskSamples[0].temperature = 35.125
	diskSamples[0].sensor.label = "New label"
	publish("disk", &disks, diskSamples, false)
	wantTemp("disk", "disk:vm-a", "35125")
	if paths("disk", "disk:vm-a")[0] != initialPath || read("disk", "disk:vm-a", "temp1_label") != "VM disk A" {
		t.Fatal("commit changed the device identity or its configured label")
	}

	t.Log("a session closed without commit has no effect")
	staged, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := staged.WriteString("sample\tdisk:vm-a\t99000\tIgnored\n")
	if err := errors.Join(writeErr, staged.Close()); err != nil {
		t.Fatal(err)
	}
	wantTemp("disk", "disk:vm-a", "35125")

	t.Log("a real ENOSPC write error is propagated without changing inventory")
	before := append([]hwmonSensor(nil), disks.sensors...)
	changed, err := publishHWMonFamily("/dev/full", "disk", &disks, diskSamples)
	if changed || !errors.Is(err, syscall.ENOSPC) || strings.Contains(err.Error(), "reconfigure") || !reflect.DeepEqual(before, disks.sensors) {
		t.Fatalf("changed=%v, err=%v, inventory=%v", changed, err, disks.sensors)
	}
	wantTemp("disk", "disk:vm-a", "35125")

	t.Log("reload loses kernel inventory; a real ESTALE triggers reconfiguration")
	command("rmmod", "virt_temp")
	command("insmod", module)
	if err := writeHWMonSamples(device, "disk", "commit", diskSamples); !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("commit after reload = %v, want ESTALE", err)
	}
	publish("disk", &disks, diskSamples, true)
	publish("hba", &hbas, hbaSamples, true)
	wantTemp("disk", "disk:vm-a", "35125")
	wantTemp("hba", "hba:vm-a", "48000")
	if read("disk", "disk:vm-a", "temp1_label") != "New label" {
		t.Fatal("reconfigure did not apply the current label")
	}

	t.Log("topology removal leaves the other family intact")
	publish("disk", &disks, nil, true)
	if len(paths("disk", "disk:vm-a")) != 0 {
		t.Fatal("removed disk still exists in sysfs")
	}
	wantTemp("hba", "hba:vm-a", "48000")
	publish("disk", &disks, diskSamples, true)

	t.Log("kernel timeout reaches failsafe, then fresh data recovers")
	if err := os.WriteFile("/sys/module/virt_temp/parameters/stale_timeout", []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for read("disk", "disk:vm-a", "temp1_input") != "100000" {
		if time.Now().After(deadline) {
			t.Fatal("kernel did not apply stale failsafe")
		}
		time.Sleep(50 * time.Millisecond)
	}
	publish("disk", &disks, diskSamples, false)
	wantTemp("disk", "disk:vm-a", "35125")

	t.Log("cached topology is restored through the real module at failsafe")
	cachePath := filepath.Join(t.TempDir(), "hwmon-inventory.json")
	cache := cachedHWMonInventory{
		Version: 1,
		Disks: &cachedHWMonFamily{Sensors: []cachedHWMonSensor{
			{ID: "disk:cached", Label: "Cached disk"},
		}},
		HBAs: &cachedHWMonFamily{Sensors: []cachedHWMonSensor{
			{ID: "hba:cached", Label: "Cached HBA"},
		}},
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	command("rmmod", "virt_temp")
	command("insmod", module)
	restored := &hwmonPublisher{cachePath: cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	wantTemp("disk", "disk:cached", "100000")
	wantTemp("hba", "hba:cached", "100000")
	if got := read("disk", "disk:cached", "name"); got != "unraid_cached_disk" {
		t.Fatalf("restored disk hwmon name = %q, want unraid_cached_disk", got)
	}
	if got := read("hba", "hba:cached", "name"); got != "unraid_cached_hba" {
		t.Fatalf("restored HBA hwmon name = %q, want unraid_cached_hba", got)
	}
}
