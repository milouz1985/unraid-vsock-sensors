// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"unraid-vsock-sensors/internal/sensors"
)

func fallbackTestCommand(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEmhttpPollHeartbeatAndFallbackRecovery(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\ntemp=35\n")
	env.report(t, "disk1", env.now)
	callLog := filepath.Join(t.TempDir(), "calls")
	env.paths.sdspin = fallbackTestCommand(t, "printf 'sdspin %s %s\\n' \"$1\" \"$2\" >> '"+callLog+"'\nexit 0")
	env.paths.smartctlType = fallbackTestCommand(t, "printf 'smart %s %s\\n' \"$1\" \"$2\" >> '"+callLog+"'\nif [ \"$(grep -c '^smart ' '"+callLog+"')\" -gt 1 ]; then exit 1; fi\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("initial native reading = %#v", disk)
	}
	if status := collector.smartSource.status(); !status.lastHeartbeat.IsZero() || status.heartbeatSeen {
		t.Fatal("collector startup was exposed as a real heartbeat")
	}
	env.now = env.now.Add(45 * time.Second)
	collector.refresh()
	if collector.smartSource.status().source == diskSourceDirect {
		t.Fatal("fallback enabled at the 45-second boundary")
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("external command invoked before stale threshold: %v", err)
	}
	env.now = env.now.Add(time.Second)
	collector.refresh()
	if collector.smartSource.status().source != diskSourceDirect {
		t.Fatal("fallback not enabled after the stale threshold")
	}
	if disk := requireSingleDisk(t, collector); disk.temp != 42 || disk.unavailable {
		t.Fatalf("fallback reading = %#v", disk)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "sdspin /dev/sda status\nsmart disk1 -n standby -A -j\n" {
		t.Fatalf("fallback calls = %q", calls)
	}
	firstCalls := string(calls)
	firstFallbackAt := env.now
	for _, elapsed := range []time.Duration{5 * time.Second, 29 * time.Second} {
		env.now = firstFallbackAt.Add(elapsed)
		collector.refresh()
		if disk := requireSingleDisk(t, collector); disk.temp != 42 || disk.unavailable {
			t.Fatalf("retained fallback reading at +%s = %#v", elapsed, disk)
		}
		unchanged, _ := os.ReadFile(callLog)
		if string(unchanged) != firstCalls {
			t.Fatalf("SMART was polled again at +%s: %q", elapsed, unchanged)
		}
	}
	env.now = env.now.Add(time.Second)
	collector.refresh()
	calls, err = os.ReadFile(callLog)
	if err != nil || string(calls) != firstCalls+firstCalls {
		t.Fatalf("second fallback poll at +30s = %q, %v", calls, err)
	}
	if disk := requireSingleDisk(t, collector); !disk.unavailable || disk.temp != 0 {
		t.Fatalf("failed second fallback poll reused the first temperature: %#v", disk)
	}
	env.now = env.now.Add(time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\ntemp=39\n")
	env.report(t, "disk1", env.now)
	collector.noteEmhttpPoll()
	collector.refresh()
	status := collector.smartSource.status()
	if status.source != diskSourceEmhttpd || !status.lastDirectAttempt.IsZero() || !status.lastHeartbeat.Equal(env.now) {
		t.Fatal("fresh emhttpd event did not restore the native source")
	}
	if disk := requireSingleDisk(t, collector); disk.temp != 39 || disk.unavailable {
		t.Fatalf("immediate native reading after recovery = %#v", disk)
	}
	callsAfter, _ := os.ReadFile(callLog)
	if string(callsAfter) != string(calls) {
		t.Fatal("fallback command ran after recovery")
	}
	env.now = env.now.Add(30 * time.Second)
	collector.refresh()
	callsAfter, _ = os.ReadFile(callLog)
	if string(callsAfter) != string(calls) {
		t.Fatal("fallback schedule survived emhttpd recovery")
	}
}

func TestFallbackStandbyUnknownAndUnsafeBuses(t *testing.T) {
	for _, scenario := range []struct {
		name, transport, sdspinExit string
		wantStandby                 bool
	}{
		{name: "ATA standby", transport: "ata", sdspinExit: "2", wantStandby: true},
		{name: "ATA unknown", transport: "ata", sdspinExit: "1"},
		{name: "SAS HDD", transport: "sas"},
		{name: "unknown HDD", transport: ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			env := newDiskTestEnvironment(t, "30")
			env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\ntransport="+scenario.transport+"\ntemp=35\n")
			callLog := filepath.Join(t.TempDir(), "calls")
			env.paths.sdspin = fallbackTestCommand(t, "printf 'sdspin\\n' >> '"+callLog+"'\nexit "+scenario.sdspinExit)
			env.paths.smartctlType = fallbackTestCommand(t, "printf 'smart\\n' >> '"+callLog+"'\nprintf '{\"temperature\":{\"current\":42}}\\n'")
			collector := env.collector()
			collector.refresh()
			env.now = env.now.Add(46 * time.Second)
			collector.refresh()
			disk := requireSingleDisk(t, collector)
			if scenario.wantStandby {
				if disk.temp != 0 || disk.unavailable {
					t.Fatalf("standby reading = %#v", disk)
				}
			} else if !disk.unavailable {
				t.Fatalf("unsafe/uncertain disk reading = %#v", disk)
			}
			calls, err := os.ReadFile(callLog)
			if scenario.transport == "ata" {
				if err != nil || string(calls) != "sdspin\n" {
					t.Fatalf("ATA calls = %q, %v", calls, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("unsafe bus invoked external command: %q, %v", calls, err)
			}
		})
	}
}

func TestFailedFallbackLeavesOldTemperatureUnavailable(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\ntemp=35\n")
	env.report(t, "disk1", env.now)
	env.paths.smartctlType = fallbackTestCommand(t, "exit 1")
	env.paths.sdspin = fallbackTestCommand(t, "exit 1")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	disk := requireSingleDisk(t, collector)
	if !disk.unavailable || disk.temp != 0 {
		t.Fatalf("failed fallback reused an old temperature: %#v", disk)
	}
	samples, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{{ID: disk.id, Name: disk.name, Device: disk.device, Transport: "nvme", Unavailable: disk.unavailable}}})
	if len(samples) != 1 || !samples[0].omitOnCommit {
		t.Fatalf("hwmon should let the 10-second failsafe expire: %#v", samples)
	}
	// A poll event without a new temperature must not re-enable the usual
	// wake-up grace and resurrect the value from before the stalled period.
	env.now = env.now.Add(time.Second)
	collector.noteEmhttpPoll()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); !disk.unavailable {
		t.Fatalf("old native cache resurrected stale temperature: %#v", disk)
	}
	env.now = env.now.Add(time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\ntemp=40\n")
	env.report(t, "disk1", env.now)
	collector.noteEmhttpPoll()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.unavailable || disk.temp != 40 {
		t.Fatalf("fresh native temperature did not recover: %#v", disk)
	}
}

func TestFailedFallbackWaitsForNextPollInterval(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\ntemp=35\n")
	env.report(t, "disk1", env.now)
	callLog := filepath.Join(t.TempDir(), "calls")
	env.paths.smartctlType = fallbackTestCommand(t, "printf 'smart\\n' >> '"+callLog+"'\nexit 1")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	firstFallbackAt := env.now
	for _, elapsed := range []time.Duration{5 * time.Second, 29 * time.Second} {
		env.now = firstFallbackAt.Add(elapsed)
		collector.refresh()
		if disk := requireSingleDisk(t, collector); !disk.unavailable || disk.temp != 0 {
			t.Fatalf("failed reading at +%s = %#v", elapsed, disk)
		}
		calls, err := os.ReadFile(callLog)
		if err != nil || string(calls) != "smart\n" {
			t.Fatalf("fallback retried too soon at +%s: %q, %v", elapsed, calls, err)
		}
	}
	env.now = firstFallbackAt.Add(30 * time.Second)
	collector.refresh()
	calls, err := os.ReadFile(callLog)
	if err != nil || string(calls) != "smart\nsmart\n" {
		t.Fatalf("fallback did not retry at +30s: %q, %v", calls, err)
	}
}

func TestFallbackUsesConfiguredPollInterval(t *testing.T) {
	env := newDiskTestEnvironment(t, "60")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\n")
	callLog := filepath.Join(t.TempDir(), "calls")
	env.paths.smartctlType = fallbackTestCommand(t, "printf 'smart\\n' >> '"+callLog+"'\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(76 * time.Second)
	collector.refresh()
	firstFallbackAt := env.now
	env.now = firstFallbackAt.Add(30 * time.Second)
	collector.refresh()
	calls, err := os.ReadFile(callLog)
	if err != nil || string(calls) != "smart\n" {
		t.Fatalf("60-second policy polled too soon: %q, %v", calls, err)
	}
	env.now = firstFallbackAt.Add(60 * time.Second)
	collector.refresh()
	calls, err = os.ReadFile(callLog)
	if err != nil || string(calls) != "smart\nsmart\n" {
		t.Fatalf("60-second policy did not poll on time: %q, %v", calls, err)
	}
}

func TestFallbackRetainedReadingsFollowInventoryWithoutReusingChangedDevice(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme0n1\ntransport=nvme\n")
	callLog := filepath.Join(t.TempDir(), "calls")
	env.paths.smartctlType = fallbackTestCommand(t, "printf '%s\\n' \"$1\" >> '"+callLog+"'\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	firstFallbackAt := env.now
	env.now = firstFallbackAt.Add(5 * time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme0n1\ntransport=nvme\n[disk2]\nid=second\ndevice=nvme1n1\ntransport=nvme\n")
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 2 || readings[0].Temp != 42 || readings[0].Unavailable || !readings[1].Unavailable {
		t.Fatalf("inventory addition between SMART polls = %#v, %v", readings, err)
	}
	env.now = firstFallbackAt.Add(10 * time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme2n1\ntransport=nvme\n")
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 || readings[0].Device != "nvme2n1" {
		t.Fatalf("changed device reused an old measurement: %#v, %v", readings, err)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil || string(calls) != "disk1\n" {
		t.Fatalf("inventory watchdog triggered extra SMART: %q, %v", calls, err)
	}
	env.now = firstFallbackAt.Add(30 * time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.unavailable || disk.temp != 42 || disk.device != "nvme2n1" {
		t.Fatalf("next direct poll did not recover changed device: %#v", disk)
	}
}

func TestDirectSMARTJSONTemperatures(t *testing.T) {
	for _, scenario := range []struct {
		report string
		want   float64
	}{
		{`{"temperature":{"current":33}}`, 33},
		{`{"nvme_smart_health_information_log":{"temperature":41}}`, 41},
		{`{"scsi_temperature":{"current":37}}`, 37},
		{`{"ata_smart_attributes":{"table":[{"id":194,"raw":{"value":"38 (Min/Max 21/40)"}}]}}`, 38},
		{`{"ata_smart_attributes":{"table":[{"id":190,"raw":{"value":588775452,"string":"29 (Min/Max 24/35)"}},{"id":194,"raw":{"value":68719476764,"string":"28 (0 16 0 0 0)"}}]}}`, 28},
		{`{"smartctl":{"exit_status":2},"power_mode":{"name":"STANDBY"}}`, 0},
		{`{"smartctl":{"exit_status":2},"temperature":{"current":42}}`, 0},
		{`{"temperature":{"current":0}}`, 0},
	} {
		got, err := parseDirectSMARTTemperature([]byte(scenario.report))
		if scenario.want == 0 {
			if err == nil {
				t.Fatalf("accepted unusable report %s", scenario.report)
			}
		} else if err != nil || got != scenario.want {
			t.Fatalf("parse %s = %v, %v", scenario.report, got, err)
		}
	}
}

func TestFallbackCommandTimeoutDoesNotBlockOtherDisks(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=slow\ndevice=nvme0n1\ntransport=nvme\n[pool1]\nid=fast\ndevice=nvme1n1\ntransport=nvme\n")
	env.paths.smartctlType = fallbackTestCommand(t, "if [ \"$1\" = disk1 ]; then sleep 10; fi\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	start := time.Now()
	collector.refresh()
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("bounded fallback took %s", elapsed)
	}
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 2 {
		t.Fatalf("snapshot = %#v, %v", readings, err)
	}
	for _, reading := range readings {
		if reading.ID == "slow" && !reading.Unavailable {
			t.Fatalf("hung command returned a temperature: %#v", reading)
		}
		if reading.ID == "fast" && (reading.Unavailable || reading.Temp != 42) {
			t.Fatalf("other disk was blocked: %#v", reading)
		}
	}
}

func TestPollAttributesZeroDoesNotEnableFallback(t *testing.T) {
	env := newDiskTestEnvironment(t, "0")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\ntemp=35\n")
	env.paths.sdspin = fallbackTestCommand(t, "exit 0")
	env.paths.smartctlType = fallbackTestCommand(t, "exit 0")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(time.Hour)
	collector.refresh()
	if collector.smartSource.status().source == diskSourceDirect {
		t.Fatal("disabled emhttpd polling enabled fallback")
	}
}

func TestFallbackCallAlwaysContainsStandbyProtection(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.devsINI, "[device1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\n")
	argsFile := filepath.Join(t.TempDir(), "args")
	env.paths.smartctlType = fallbackTestCommand(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\nprintf '{\"temperature\":{\"current\":44}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	args, err := os.ReadFile(argsFile)
	if err != nil || strings.TrimSpace(string(args)) != "device1 -n standby -A -j" {
		t.Fatalf("smartctl_type args = %q, %v", args, err)
	}
}

func TestFallbackCommandCancellation(t *testing.T) {
	command := fallbackTestCommand(t, "sleep 10")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := runFallbackCommand(ctx, command); err == nil || time.Since(start) > time.Second {
		t.Fatalf("canceled command was not bounded: %v", err)
	}
}

func TestFallbackConcurrencyIsBounded(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	var inventory strings.Builder
	for i := range 6 {
		inventory.WriteString("[disk" + strconv.Itoa(i+1) + "]\nid=serial" + strconv.Itoa(i+1) + "\ndevice=nvme" + strconv.Itoa(i) + "n1\ntransport=nvme\n")
	}
	env.write(t, env.paths.disksINI, inventory.String())
	callLog := filepath.Join(t.TempDir(), "starts")
	release := filepath.Join(t.TempDir(), "release")
	env.paths.smartctlType = fallbackTestCommand(t,
		"printf '%s\\n' \"$1\" >> '"+callLog+"'\n"+
			"while [ ! -e '"+release+"' ]; do sleep 0.01; done\n"+
			"printf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	done := make(chan struct{})
	go func() { collector.refresh(); close(done) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(callLog)
		if len(strings.Fields(string(data))) == fallbackWorkers {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, _ := os.ReadFile(callLog)
	if got := len(strings.Fields(string(data))); got != fallbackWorkers {
		t.Fatalf("started %d commands before release, want %d", got, fallbackWorkers)
	}
	time.Sleep(50 * time.Millisecond)
	data, _ = os.ReadFile(callLog)
	if got := len(strings.Fields(string(data))); got != fallbackWorkers {
		t.Fatalf("started %d commands while first workers were busy", got)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded workers did not finish")
	}
	data, _ = os.ReadFile(callLog)
	if got := len(strings.Fields(string(data))); got != 6 {
		t.Fatalf("processed %d of 6 disks", got)
	}
}

func TestOnlyEmhttpPollSignalAdvancesHeartbeat(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	collector := env.collector()
	manual := make(chan os.Signal, 1)
	poll := make(chan os.Signal, 1)
	refresh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwardDiskRefreshSignals(ctx, manual, poll, refresh, collector)
	manual <- unix.SIGUSR1
	select {
	case <-refresh:
	case <-time.After(time.Second):
		t.Fatal("manual refresh signal was not forwarded")
	}
	if collector.smartSource.status().heartbeatSeen {
		t.Fatal("manual refresh advanced emhttpd heartbeat")
	}
	poll <- unix.SIGUSR2
	select {
	case <-refresh:
	case <-time.After(time.Second):
		t.Fatal("emhttpd poll signal was not forwarded")
	}
	if got := collector.smartSource.status().lastHeartbeat; !got.Equal(env.now) {
		t.Fatalf("emhttpd heartbeat = %v, want %v", got, env.now)
	}
}
