// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePollAttributes(t *testing.T) {
	for name, test := range map[string]struct {
		data string
		want time.Duration
		err  bool
	}{
		"positive":    {data: "poll_attributes=\"30\"\n", want: 30 * time.Second},
		"zero":        {data: "poll_attributes=\"0\"\n", want: 0},
		"missing":     {data: "other=\"30\"\n", err: true},
		"negative":    {data: "poll_attributes=\"-1\"\n", err: true},
		"not numeric": {data: "poll_attributes=\"fast\"\n", err: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePollAttributes([]byte(test.data))
			if got != test.want || (err != nil) != test.err {
				t.Fatalf("parse = %s, %v; want %s, error=%v", got, err, test.want, test.err)
			}
		})
	}
}

func TestCurrentUnraidVarFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "unraid", "current", "var.ini"))
	if err != nil {
		t.Fatal(err)
	}
	interval, err := parsePollAttributes(data)
	if err != nil || interval != 30*time.Second {
		t.Fatalf("poll attributes interval = %s, %v; want 30s", interval, err)
	}
}

func TestReadPollAttributesAlwaysReadsCurrentContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var.ini")
	mtime := time.Unix(1_800_000_000, 0)
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("poll_attributes=\""+value+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	write("30")
	if interval, err := readPollAttributes(path); interval != 30*time.Second || err != nil {
		t.Fatalf("initial read = %s, %v", interval, err)
	}

	write("60")
	if interval, err := readPollAttributes(path); interval != 60*time.Second || err != nil {
		t.Fatalf("same-metadata read = %s, %v", interval, err)
	}

	write("xx")
	if interval, err := readPollAttributes(path); interval != defaultPollAttributes || err == nil {
		t.Fatalf("invalid read = %s, %v", interval, err)
	}
	write("90")
	if interval, err := readPollAttributes(path); interval != 90*time.Second || err != nil {
		t.Fatalf("read after parse error = %s, %v", interval, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if interval, err := readPollAttributes(path); interval != defaultPollAttributes || err == nil {
		t.Fatalf("missing read = %s, %v", interval, err)
	}
	write("45")
	if interval, err := readPollAttributes(path); interval != 45*time.Second || err != nil {
		t.Fatalf("read after file error = %s, %v", interval, err)
	}
}

func TestDiskCollectorKeepsLastValidPollAttributes(t *testing.T) {
	environment := newDiskTestEnvironment(t, "60")
	collector := environment.collector()

	collector.refresh()
	status := collector.smartSource.status()
	if status.pollInterval != 60*time.Second || status.configError != "" {
		t.Fatalf("initial poll status = %+v", status)
	}

	environment.write(t, environment.paths.varINI, "poll_attributes=\"invalid\"\n")
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	status = collector.smartSource.status()
	if status.pollInterval != 60*time.Second || status.configError == "" {
		t.Fatalf("status after invalid config = %+v; want retained 60s with error", status)
	}

	environment.write(t, environment.paths.varINI, "poll_attributes=\"0\"\n")
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	status = collector.smartSource.status()
	if status.pollInterval != 0 || status.configError != "" {
		t.Fatalf("status after valid zero = %+v", status)
	}

	environment.write(t, environment.paths.varINI, "poll_attributes=\"invalid\"\n")
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	status = collector.smartSource.status()
	if status.pollInterval != 0 || status.configError == "" {
		t.Fatalf("status after invalid config following zero = %+v; want retained zero with error", status)
	}
}

func TestDiskCollectorUsesDefaultPollAttributesUntilFirstValidRead(t *testing.T) {
	environment := newDiskTestEnvironment(t, "invalid")
	collector := environment.collector()

	collector.refresh()
	status := collector.smartSource.status()
	if status.pollInterval != defaultPollAttributes || status.configError == "" {
		t.Fatalf("cold-start poll status = %+v; want default interval with error", status)
	}

	environment.write(t, environment.paths.varINI, "poll_attributes=\"45\"\n")
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	status = collector.smartSource.status()
	if status.pollInterval != 45*time.Second || status.configError != "" {
		t.Fatalf("status after first valid read = %+v", status)
	}
}

func TestPollAttributesLogMessageWithoutSMARTCache(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	logPollAttributes(defaultPollAttributes, errors.New("invalid config"))
	if message := output.String(); !strings.Contains(message, "invalid config") || !strings.Contains(message, "30s") || !strings.Contains(message, "stalled-poll detection") {
		t.Fatalf("fallback warning = %q", message)
	}

	output.Reset()
	logPollAttributes(5*time.Minute, errors.New("invalid config"))
	if message := output.String(); !strings.Contains(message, "5m0s") {
		t.Fatalf("last-known-good warning = %q", message)
	}
}

func TestPollAttributesWarnings(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	logPollAttributes(0, nil)
	if message := output.String(); !strings.Contains(message, "poll_attributes=0") || !strings.Contains(message, "disabled") {
		t.Fatalf("disabled warning = %q", message)
	}
	output.Reset()
	logPollAttributes(5*time.Minute, nil)
	if message := output.String(); !strings.Contains(message, "5m0s") || !strings.Contains(message, "fan control") {
		t.Fatalf("slow polling warning = %q", message)
	}
	output.Reset()
	logPollAttributes(defaultPollAttributes, errors.New("invalid config"))
	if message := output.String(); !strings.Contains(message, "invalid config") || !strings.Contains(message, "30s for stalled-poll detection") {
		t.Fatalf("fallback warning = %q", message)
	}
}
