// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatDuringDirectSMARTCollection(t *testing.T) {
	env := newDiskTestEnvironment(t, "30")
	env.write(t, env.paths.disksINI, "[disk1]\nid=serial\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=39\n")
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	commandLog := filepath.Join(t.TempDir(), "commands")
	env.paths.smartctlType = fallbackTestCommand(t, "printf 'smart\\n' >> '"+commandLog+"'\ntouch '"+started+"'\nwhile [ ! -e '"+release+"' ]; do sleep 0.01; done\nprintf '{\"temperature\":{\"current\":42}}\\n'")
	collector := env.collector()
	collector.refresh()
	env.now = env.now.Add(46 * time.Second)
	done := make(chan struct{})
	go func() {
		collector.refresh()
		close(done)
	}()
	deadline := time.After(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("direct SMART command did not start")
		case <-time.After(time.Millisecond):
		}
	}
	heartbeatDone := make(chan struct{})
	go func() {
		collector.noteEmhttpPoll()
		close(heartbeatDone)
	}()
	select {
	case <-heartbeatDone:
	case <-time.After(time.Second):
		t.Fatal("heartbeat blocked on direct SMART I/O")
	}
	if status := collector.smartSource.status(); !status.heartbeatSeen || !status.lastHeartbeat.Equal(env.now) {
		t.Fatalf("heartbeat was not recorded during collection: %+v", status)
	}
	env.write(t, release, "")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("direct SMART collection did not finish")
	}
	if status := collector.status(); status.source.source != diskSourceDirect {
		t.Fatalf("in-flight refresh changed source: %+v", status.source)
	}
	env.now = env.now.Add(time.Second)
	collector.refresh()
	if status := collector.status(); status.source.source != diskSourceEmhttpd {
		t.Fatalf("next refresh did not recover emhttpd: %+v", status.source)
	}
	if disk := requireSingleDisk(t, collector); disk.temp != 39 || disk.unavailable {
		t.Fatalf("native reading after recovery = %#v", disk)
	}
	env.now = env.now.Add(5 * time.Second)
	collector.refresh()
	commands, err := os.ReadFile(commandLog)
	if err != nil || string(commands) != "smart\n" {
		t.Fatalf("old fallback cadence survived recovery: %q, %v", commands, err)
	}
}
