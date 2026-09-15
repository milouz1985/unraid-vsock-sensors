// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSmartSourceStateLifecycleAndAttemptCadence(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var state smartSourceState

	decision := state.evaluate(start, 30*time.Second, nil)
	if decision.source != diskSourceEmhttpd || decision.directDue {
		t.Fatalf("startup decision = %+v", decision)
	}
	status := state.status()
	if status.heartbeatSeen || !status.lastHeartbeat.IsZero() {
		t.Fatalf("startup invented a heartbeat: %+v", status)
	}

	state.noteEmhttpPoll(start.Add(10 * time.Second))
	decision = state.evaluate(start.Add(55*time.Second), 30*time.Second, nil)
	if decision.source != diskSourceEmhttpd || decision.directDue {
		t.Fatalf("boundary decision = %+v", decision)
	}

	enteredAt := start.Add(56 * time.Second)
	decision = state.evaluate(enteredAt, 30*time.Second, nil)
	if decision.source != diskSourceDirect || !decision.justEntered || !decision.directDue {
		t.Fatalf("fallback entry = %+v", decision)
	}
	state.beginDirectAttempt(enteredAt)
	if decision = state.evaluate(enteredAt.Add(29*time.Second), 30*time.Second, nil); decision.directDue {
		t.Fatalf("attempt became due early: %+v", decision)
	}
	if decision = state.evaluate(enteredAt.Add(30*time.Second), 30*time.Second, nil); !decision.directDue {
		t.Fatalf("attempt not due on cadence: %+v", decision)
	}
	state.beginDirectAttempt(enteredAt.Add(30 * time.Second))

	state.recordFallbackResult(enteredAt.Add(30*time.Second), errors.New("SMART failed"))
	state.noteEmhttpPoll(enteredAt.Add(31 * time.Second))
	decision = state.evaluate(enteredAt.Add(31*time.Second), 30*time.Second, nil)
	status = state.status()
	if decision.source != diskSourceEmhttpd || !decision.justRecovered ||
		!status.lastObservedAttempt.Equal(enteredAt.Add(30*time.Second)) || status.lastFallbackError != "" {
		t.Fatalf("fallback recovery = %+v, status=%+v", decision, status)
	}
	reenteredAt := enteredAt.Add(77 * time.Second)
	decision = state.evaluate(reenteredAt, 30*time.Second, nil)
	if decision.source != diskSourceDirect || !decision.justEntered || !decision.directDue {
		t.Fatalf("new fallback entry did not schedule an immediate attempt: %+v", decision)
	}
}

func TestSmartSourceDueDecisionDoesNotConsumeAttempt(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var state smartSourceState
	state.evaluate(start, 30*time.Second, nil)
	firstDue := start.Add(46 * time.Second)
	if decision := state.evaluate(firstDue, 30*time.Second, nil); !decision.directDue {
		t.Fatalf("first direct collection not due: %+v", decision)
	}
	// Inventory can fail after the decision. Until collection really starts,
	// the next refresh must still be allowed to perform the immediate attempt.
	if decision := state.evaluate(firstDue.Add(time.Second), 30*time.Second, nil); !decision.directDue {
		t.Fatalf("unstarted attempt was consumed: %+v", decision)
	}
}

func TestSmartSourceStateDisabledPollingAndConfigError(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var state smartSourceState
	configErr := errors.New("invalid poll_attributes")
	state.evaluate(start, defaultPollAttributes, configErr)
	if got := state.status(); got.configError != configErr.Error() || got.pollInterval != defaultPollAttributes {
		t.Fatalf("configuration status = %+v", got)
	}
	if decision := state.evaluate(start.Add(24*time.Hour), 0, nil); decision.source != diskSourceEmhttpd || decision.directDue {
		t.Fatalf("disabled polling enabled fallback: %+v", decision)
	}
}

func TestSmartSourceStateConcurrentHeartbeatAndEvaluation(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var state smartSourceState
	state.evaluate(start, 30*time.Second, nil)

	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func(offset int) {
			defer workers.Done()
			for iteration := range 1_000 {
				now := start.Add(time.Duration(offset*1_000+iteration) * time.Millisecond)
				if iteration%2 == 0 {
					state.noteEmhttpPoll(now)
				} else {
					state.evaluate(now, 30*time.Second, nil)
				}
			}
		}(worker)
	}
	workers.Wait()
	if status := state.status(); !status.initialized || !status.heartbeatSeen {
		t.Fatalf("concurrent state lost updates: %+v", status)
	}
}
