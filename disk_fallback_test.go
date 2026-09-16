// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"unraid-vsock-sensors/internal/sensors"
)

func TestFallbackAdvancesPastSlowDisksAcrossCycles(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "attempts")
	command := fallbackTestCommand(t, "printf '%s\\n' \"$1\" >> '"+logPath+"'\ncase \"$1\" in fast) printf '{\"temperature\":{\"current\":42}}\\n' ;; *) sleep 0.2 ;; esac")
	collector := newDiskCollector(diskDataPaths{smartctlType: command})
	disks := make([]unraidDisk, 0, 7)
	for i := range 6 {
		disks = append(disks, unraidDisk{name: "slow" + strconv.Itoa(i), id: "slow" + strconv.Itoa(i)})
	}
	disks = append(disks, unraidDisk{name: "fast", id: "fast"})
	for cycle := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		observations, _ := collector.collectFallback(ctx, disks)
		cancel()
		attempts, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if cycle == 0 && strings.Contains(string(attempts), "fast\n") {
			t.Fatal("fast disk was unexpectedly reached in first constrained cycle")
		}
		if cycle == 2 {
			if !strings.Contains(string(attempts), "fast\n") || observations[6].temperature != 42 || observations[6].err != nil {
				t.Fatalf("fast disk starved across cycles: attempts %q, observation %#v", attempts, observations[6])
			}
		}
	}
}

func TestFallbackCursorFollowsChangedInventories(t *testing.T) {
	for _, test := range []struct {
		name   string
		cursor int
		disks  []string
		want   []string
	}{
		{name: "removed disk", cursor: 3, disks: []string{"a", "b", "c", "d"}, want: []string{"d", "a", "b"}},
		{name: "added disk", cursor: 3, disks: []string{"a", "b", "c", "d", "e"}, want: []string{"d", "e", "a"}},
		{name: "empty inventory", cursor: 3},
		{name: "cursor beyond new length", cursor: 8, disks: []string{"a", "b", "c", "d"}, want: []string{"a", "b", "c"}},
		{name: "reordered inventory", cursor: 3, disks: []string{"d", "c", "b", "a", "e"}, want: []string{"a", "e", "d"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "attempts")
			command := fallbackTestCommand(t, "printf '%s\\n' \"$1\" >> '"+logPath+"'\nsleep 0.2")
			collector := newDiskCollector(diskDataPaths{smartctlType: command})
			collector.fallbackCursor = test.cursor
			disks := make([]unraidDisk, len(test.disks))
			for i, name := range test.disks {
				disks[i] = unraidDisk{name: name, id: name}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, _ = collector.collectFallback(ctx, disks)
			cancel()
			if len(disks) == 0 {
				if collector.fallbackCursor != 0 {
					t.Fatalf("cursor after empty inventory = %d", collector.fallbackCursor)
				}
				return
			}
			attempts, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Fields(string(attempts))
			slices.Sort(got)
			want := slices.Clone(test.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("attempts = %q; want %q", got, want)
			}
		})
	}
}

func fallbackTestCommand(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiskRefreshSeparatesDecisionAndFinishedTimes(t *testing.T) {
	decisionAt := time.Unix(1_800_000_100, 0)
	finishedAt := decisionAt.Add(4 * time.Second)

	t.Run("successful direct reading", func(t *testing.T) {
		env := newDiskTestEnvironment(t, "30")
		env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n")
		env.paths.smartctlType = fallbackTestCommand(t, "printf '{\"temperature\":{\"current\":42}}\\n'")
		collector := env.collector()
		collector.smartSource.evaluate(decisionAt.Add(-46*time.Second), 30*time.Second, nil)
		calls := 0
		collector.now = func() time.Time {
			calls++
			if calls == 1 {
				return decisionAt
			}
			return finishedAt
		}

		collector.refresh()
		status := collector.status()
		if !collector.smartSource.lastDirectAttempt.Equal(decisionAt) ||
			!status.source.lastFallbackAttempt.Equal(decisionAt) {
			t.Fatalf("attempt timestamps = scheduler %s, diagnostic %s; want %s",
				collector.smartSource.lastDirectAttempt, status.source.lastFallbackAttempt, decisionAt)
		}
		if !status.updatedAt.Equal(finishedAt) || !collector.state["serial"].lastValidAt.Equal(finishedAt) {
			t.Fatalf("completion timestamps = snapshot %s, reading %s; want %s",
				status.updatedAt, collector.state["serial"].lastValidAt, finishedAt)
		}
	})

	t.Run("failed direct reading", func(t *testing.T) {
		env := newDiskTestEnvironment(t, "30")
		env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n")
		env.paths.smartctlType = fallbackTestCommand(t, "exit 1")
		collector := env.collector()
		collector.smartSource.evaluate(decisionAt.Add(-46*time.Second), 30*time.Second, nil)
		calls := 0
		collector.now = func() time.Time {
			calls++
			if calls == 1 {
				return decisionAt
			}
			return finishedAt
		}

		collector.refresh()
		status := collector.status()
		if !status.updatedAt.Equal(finishedAt) || !status.source.fallbackErrorAt.Equal(finishedAt) {
			t.Fatalf("failure timestamps = snapshot %s, fallback error %s; want %s",
				status.updatedAt, status.source.fallbackErrorAt, finishedAt)
		}
	})

	t.Run("inventory failure", func(t *testing.T) {
		env := newDiskTestEnvironment(t, "30")
		if err := os.Remove(env.paths.disksINI); err != nil {
			t.Fatal(err)
		}
		collector := env.collector()
		calls := 0
		collector.now = func() time.Time {
			calls++
			if calls == 1 {
				return decisionAt
			}
			return finishedAt
		}

		collector.refresh()
		if status := collector.status(); !status.errorAt.Equal(finishedAt) {
			t.Fatalf("inventory error timestamp = %s; want %s", status.errorAt, finishedAt)
		}
	})
}

func TestEmhttpPollHeartbeatAndFallbackRecovery(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
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
	if string(calls) != "sdspin /dev/sda status\nsmart disk1 -n standby,3 -A -j\n" {
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
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=39\n")
	collector.noteEmhttpPoll()
	collector.refresh()
	status := collector.smartSource.status()
	if status.source != diskSourceEmhttpd || !status.lastHeartbeat.Equal(env.now) {
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
	env.now = env.now.Add(16 * time.Second)
	collector.refresh()
	callsAfter, err = os.ReadFile(callLog)
	if collector.smartSource.status().source != diskSourceDirect || err != nil || string(callsAfter) != string(calls)+firstCalls {
		t.Fatalf("new fallback entry did not trigger an immediate attempt: %q, %v", callsAfter, err)
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
			env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntransport="+scenario.transport+"\ntemp=35\n")
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

func TestFallbackSmartctlExitCodesAndResults(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		exitCode    int
		output      string
		wantTemp    float64
		wantStandby bool
		wantError   string
	}{
		{name: "exit 0", output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "exit 1", exitCode: 1, output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "exit 2", exitCode: 2, output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "standby exit 3", exitCode: 3, output: `{"temperature":{"current":42}}`, wantStandby: true},
		{name: "exit 4", exitCode: 4, output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "health bit exit 8", exitCode: 8, output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "combined exit 12", exitCode: 12, output: `{"temperature":{"current":42}}`, wantTemp: 42},
		{name: "nonzero without temperature", exitCode: 4, output: `{}`, wantError: "smartctl_type exit 4: direct SMART report has no usable temperature"},
		{name: "power mode standby", output: `{"power_mode":{"name":"standby"}}`, wantStandby: true},
		{name: "invalid JSON", output: `{`, wantError: "parse direct SMART JSON"},
		{name: "nonzero invalid JSON", exitCode: 8, output: `{`, wantError: "smartctl_type exit 8: parse direct SMART JSON"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			env := newDiskTestEnvironment(t, "30")
			env.paths.sdspin = fallbackTestCommand(t, "exit 0")
			env.paths.smartctlType = fallbackTestCommand(t,
				"printf '%s\\n' '"+scenario.output+"'\nexit "+strconv.Itoa(scenario.exitCode))
			collector := env.collector()
			observation := collector.fallbackObservation(context.Background(), unraidDisk{
				id: "serial", name: "disk1", device: "sda", transport: "ata", rotational: true,
			})
			if observation.temperature != scenario.wantTemp || observation.standby != scenario.wantStandby {
				t.Fatalf("observation = %+v", observation)
			}
			if scenario.wantError == "" && observation.err != nil {
				t.Fatalf("unexpected error: %v", observation.err)
			}
			if scenario.wantError != "" && (observation.err == nil || !strings.Contains(observation.err.Error(), scenario.wantError)) {
				t.Fatalf("error = %v, want it to contain %q", observation.err, scenario.wantError)
			}
		})
	}
}

func TestFailedFallbackLeavesOldTemperatureUnavailable(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=35\n")
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
	if state := collector.state["serial"]; state.thermalState != diskThermalUnavailable {
		t.Fatalf("failed fallback state = %#v", state)
	}
	samples, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{{ID: disk.id, Name: disk.name, Device: disk.device, Transport: "nvme", Unavailable: disk.unavailable}}})
	if len(samples) != 1 || !samples[0].omitOnCommit {
		t.Fatalf("hwmon should let the 10-second failsafe expire: %#v", samples)
	}
	// A poll event without a new temperature must not re-enable the usual
	// wake-up grace. With emhttpd as authority, the same numeric value is
	// accepted as a valid reading (UVSS does not determine if the value changed).
	env.now = env.now.Add(time.Second)
	collector.noteEmhttpPoll()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.unavailable || disk.temp != 35 {
		t.Fatalf("emhttpd reading after fallback failure: %#v; want temp 35 available", disk)
	}
	env.now = env.now.Add(time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=40\n")
	collector.noteEmhttpPoll()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.unavailable || disk.temp != 40 {
		t.Fatalf("fresh native temperature did not recover: %#v", disk)
	}
}

func TestCollectionErrorPreservedBetweenDirectPolls(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=35\n")
	env.paths.smartctlType = fallbackTestCommand(t, "exit 1")
	collector := env.collector()
	collector.refresh()

	// Trigger fallback with a failure.
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); !disk.unavailable {
		t.Fatalf("disk after failed fallback = %#v; want unavailable", disk)
	}
	// The collection error must be present in the runtime snapshot.
	status := collector.status()
	if len(status.disks) != 1 || status.disks[0].collectionError == nil {
		t.Fatalf("collection error not recorded: %+v", status.disks)
	}
	initialError := status.disks[0].collectionError.Error()

	// Watchdog tick before the next direct poll (poll_attributes=30s, so
	// 5s later is still within the reuse window).
	env.now = env.now.Add(5 * time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); !disk.unavailable {
		t.Fatalf("disk during reuse = %#v; want still unavailable", disk)
	}
	// The collection error must be preserved.
	status = collector.status()
	if len(status.disks) != 1 || status.disks[0].collectionError == nil {
		t.Fatalf("collection error lost during reuse: %+v", status.disks)
	}
	if status.disks[0].collectionError.Error() != initialError {
		t.Fatalf("collection error changed during reuse: was %q, now %q",
			initialError, status.disks[0].collectionError.Error())
	}
}

func TestCollectionErrorPreservedAfterDeviceChangeForSameID(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=35\n")
	env.paths.smartctlType = fallbackTestCommand(t, "exit 1")
	collector := env.collector()
	collector.refresh()

	// Trigger fallback with a failure on nvme0n1.
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); !disk.unavailable {
		t.Fatalf("disk after failed fallback = %#v; want unavailable", disk)
	}
	status := collector.status()
	if len(status.disks) != 1 || status.disks[0].collectionError == nil {
		t.Fatalf("collection error not recorded: %+v", status.disks)
	}
	initialError := status.disks[0].collectionError.Error()

	// Device changes to nvme2n1.
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme2n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=35\n")
	env.now = env.now.Add(5 * time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); !disk.unavailable {
		t.Fatalf("disk after device change = %#v; want unavailable", disk)
	}
	// The stable ID still refers to the same disk between direct polls.
	status = collector.status()
	if len(status.disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(status.disks))
	}
	if status.disks[0].collectionError == nil || status.disks[0].collectionError.Error() != initialError {
		t.Fatalf("collection error lost after device change: %v", status.disks[0].collectionError)
	}
}

func TestFailedFallbackWaitsForNextPollInterval(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=35\n")
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
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n")
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

func TestFallbackRetainedReadingsFollowStableIDsAcrossDeviceChange(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n")
	callLog := filepath.Join(t.TempDir(), "calls")
	env.paths.smartctlType = fallbackTestCommand(t, "printf '%s\\n' \"$1\" >> '"+callLog+"'\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	firstFallbackAt := env.now
	env.now = firstFallbackAt.Add(5 * time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n[disk2]\nid=second\ndevice=nvme1n1\ntransport=nvme\nrotational=0\nspundown=0\n")
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 2 || readings[0].Temp != 42 || readings[0].Unavailable || !readings[1].Unavailable {
		t.Fatalf("inventory addition between SMART polls = %#v, %v", readings, err)
	}
	env.now = firstFallbackAt.Add(10 * time.Second)
	env.write(t, env.paths.disksINI, "[disk1]\nid=first\ndevice=nvme2n1\ntransport=nvme\nrotational=0\nspundown=0\n")
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 1 || readings[0].Unavailable || readings[0].Temp != 42 || readings[0].Device != "nvme2n1" {
		t.Fatalf("stable ID lost its retained measurement after device change: %#v, %v", readings, err)
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

func TestFallbackReuseUsesStableIDAcrossTransportChange(t *testing.T) {
	collector := newDiskCollector(diskDataPaths{})
	collector.err = nil
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{
		{disk: unraidDisk{id: "first", device: "nvme0n1", transport: "nvme"}, reading: sensors.Disk{ID: "first", Device: "nvme0n1", Transport: "nvme", Temp: 41}, hasReading: true},
		{disk: unraidDisk{id: "second", device: "nvme1n1", transport: "nvme"}, reading: sensors.Disk{ID: "second", Device: "nvme1n1", Transport: "nvme", Temp: 52}, hasReading: true},
	}
	collector.state = diskStateTracker{"first": {}, "second": {}}

	readings := collector.reuseFallbackReadings([]unraidDisk{
		{id: "second", device: "nvme1n1", transport: "nvme"},
		{id: "first", device: "nvme0n1", transport: "nvme"},
	})
	if len(readings) != 2 || readings[0].ID != "second" || readings[0].Temp != 52 || readings[1].ID != "first" || readings[1].Temp != 41 {
		t.Fatalf("readings were associated by position: %#v", readings)
	}

	readings = collector.reuseFallbackReadings([]unraidDisk{{
		id: "first", device: "nvme0n1", transport: "ata",
	}})
	if len(readings) != 1 || readings[0].Unavailable || readings[0].Temp != 41 || readings[0].Transport != "ata" {
		t.Fatalf("stable ID lost its retained measurement after transport change: %#v", readings)
	}
	if _, exists := collector.state["first"]; !exists {
		t.Fatal("stable ID lost its thermal history after transport change")
	}

	readings = collector.reuseFallbackReadings([]unraidDisk{{
		id: "replacement", device: "nvme0n1", transport: "nvme",
	}})
	if len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("different ID reused an old measurement: %#v", readings)
	}
	if _, exists := collector.state["first"]; exists {
		t.Fatal("removed stable ID retained its thermal history")
	}
}

func TestFallbackReuseDoesNotReviveSnapshotAfterCollectionFailure(t *testing.T) {
	collector := newDiskCollector(diskDataPaths{})
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{{
		disk:       unraidDisk{id: "first", device: "nvme0n1", transport: "nvme"},
		reading:    sensors.Disk{ID: "first", Device: "nvme0n1", Transport: "nvme", Temp: 41},
		hasReading: true,
	}}
	collector.err = errors.New("inventory failed")
	wantState := diskState{
		thermalState: diskThermalUnavailable,
		lastValidAt:  time.Unix(1_800_000_000, 0),
		lastSource:   diskSourceDirect,
	}
	collector.state = diskStateTracker{"first": wantState}

	readings := collector.reuseFallbackReadings([]unraidDisk{{
		id: "first", device: "nvme0n1", transport: "nvme",
	}})
	if len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("failed collection revived the previous snapshot: %#v", readings)
	}
	if got := collector.state["first"]; got != wantState {
		t.Fatalf("failed collection erased thermal history: %#v; want %#v", got, wantState)
	}
}

func TestDirectSMARTJSONResults(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		report      string
		want        float64
		wantStandby bool
		wantError   bool
	}{
		{name: "generic", report: `{"temperature":{"current":33}}`, want: 33},
		{name: "structured zero", report: `{"temperature":{"current":0}}`, want: 0},
		{name: "structured negative", report: `{"temperature":{"current":-40}}`, want: -40},
		{name: "structured above 150", report: `{"temperature":{"current":200}}`, want: 200},
		{name: "ATA structured", report: `{"temperature":{"current":31},"ata_smart_attributes":{"table":[{"id":194,"raw":{"value":99,"string":"99"}}]}}`, want: 31},
		{name: "NVMe", report: `{"nvme_smart_health_information_log":{"temperature":41}}`, want: 41},
		{name: "SCSI", report: `{"scsi_temperature":{"current":37}}`, want: 37},
		{name: "ATA raw value ignored", report: `{"ata_smart_attributes":{"table":[{"id":194,"raw":{"value":29}}]}}`, wantError: true},
		{name: "ATA raw string ignored", report: `{"ata_smart_attributes":{"table":[{"id":190,"raw":{"string":"29 (Min/Max 24/35)"}},{"id":194,"raw":{"string":"28 (0 16 0 0 0)"}}]}}`, wantError: true},
		{name: "standby power mode", report: `{"smartctl":{"exit_status":2},"power_mode":{"name":"STANDBY"}}`, wantStandby: true},
		{name: "embedded smartctl bitmask ignored", report: `{"smartctl":{"exit_status":2},"temperature":{"current":42}}`, want: 42},
		{name: "invalid JSON", report: `{`, wantError: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := parseDirectSMART([]byte(scenario.report))
			if (err != nil) != scenario.wantError || got.temperature != scenario.want || got.standby != scenario.wantStandby {
				t.Fatalf("parse %s = %+v, %v", scenario.report, got, err)
			}
		})
	}
}

func TestFallbackCommandTimeoutDoesNotBlockOtherDisks(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=slow\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n[pool1]\nid=fast\ndevice=nvme1n1\ntransport=nvme\nrotational=0\nspundown=0\n")
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
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
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
	env.write(t, env.paths.devsINI, "[device1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\n")
	argsFile := filepath.Join(t.TempDir(), "args")
	env.paths.smartctlType = fallbackTestCommand(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\nprintf '{\"temperature\":{\"current\":44}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	collector.refresh()
	args, err := os.ReadFile(argsFile)
	if err != nil || strings.TrimSpace(string(args)) != "device1 -n standby,3 -A -j" {
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
		inventory.WriteString("[disk" + strconv.Itoa(i+1) + "]\nid=serial" + strconv.Itoa(i+1) + "\ndevice=nvme" + strconv.Itoa(i) + "n1\ntransport=nvme\nrotational=0\nspundown=0\n")
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
