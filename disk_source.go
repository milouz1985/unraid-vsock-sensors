// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"sync"
	"time"
)

type diskTemperatureSource string

const (
	diskSourceEmhttpd diskTemperatureSource = "emhttpd"
	diskSourceDirect  diskTemperatureSource = "direct SMART fallback"
	emhttpPollMargin                        = 15 * time.Second
)

type smartSourceDecision struct {
	source        diskTemperatureSource
	directDue     bool
	justEntered   bool
	justRecovered bool
}

type smartSourceStatus struct {
	pollInterval        time.Duration
	configError         string
	initialized         bool
	heartbeatSeen       bool
	lastHeartbeat       time.Time
	source              diskTemperatureSource
	fallbackSince       time.Time
	lastFallbackAttempt time.Time
	lastFallbackError   string
	fallbackErrorAt     time.Time
}

// smartSourceState owns heartbeat detection and direct-SMART scheduling. Its
// mutex is independent from diskCollector because an emhttpd poll notification
// may arrive while a disk collection is doing file or command I/O.
type smartSourceState struct {
	mu sync.RWMutex

	initialized         bool
	startedAt           time.Time
	heartbeatSeen       bool
	lastHeartbeat       time.Time
	fallbackActive      bool
	fallbackSince       time.Time
	lastDirectAttempt   time.Time
	lastFallbackAttempt time.Time
	pollInterval        time.Duration
	configError         string
	lastFallbackError   string
	fallbackErrorAt     time.Time
}

func (s *smartSourceState) noteEmhttpPoll(now time.Time) {
	s.mu.Lock()
	s.lastHeartbeat = now
	s.heartbeatSeen = true
	s.mu.Unlock()
}

func (s *smartSourceState) evaluate(now time.Time, pollInterval time.Duration, configErr error) smartSourceDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.initialized {
		s.startedAt = now
		s.initialized = true
	}
	s.pollInterval = pollInterval
	s.configError = errorText(configErr)

	reference := s.startedAt
	if s.heartbeatSeen {
		reference = s.lastHeartbeat
	}
	fallback := pollInterval > 0 && now.Sub(reference) > pollInterval+emhttpPollMargin
	decision := smartSourceDecision{source: diskSourceEmhttpd}
	if fallback {
		decision.source = diskSourceDirect
	}
	if fallback != s.fallbackActive {
		s.fallbackActive = fallback
		if fallback {
			s.fallbackSince = now
			decision.justEntered = true
		} else {
			s.fallbackSince = time.Time{}
			s.lastDirectAttempt = time.Time{}
			s.lastFallbackError = ""
			s.fallbackErrorAt = time.Time{}
			decision.justRecovered = true
		}
	}
	if fallback && (s.lastDirectAttempt.IsZero() || now.Sub(s.lastDirectAttempt) >= pollInterval) {
		decision.directDue = true
	}
	return decision
}

func (s *smartSourceState) beginDirectAttempt(now time.Time) {
	s.mu.Lock()
	s.lastDirectAttempt = now
	s.lastFallbackAttempt = now
	s.mu.Unlock()
}

func (s *smartSourceState) recordFallbackResult(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := errorText(err)
	if message == s.lastFallbackError {
		return
	}
	s.lastFallbackError = message
	s.fallbackErrorAt = time.Time{}
	if message != "" {
		s.fallbackErrorAt = now
	}
}

func (s *smartSourceState) status() smartSourceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	source := diskSourceEmhttpd
	if s.fallbackActive {
		source = diskSourceDirect
	}
	return smartSourceStatus{
		pollInterval: s.pollInterval, configError: s.configError, initialized: s.initialized,
		heartbeatSeen: s.heartbeatSeen, lastHeartbeat: s.lastHeartbeat, source: source,
		fallbackSince:       s.fallbackSince,
		lastFallbackAttempt: s.lastFallbackAttempt,
		lastFallbackError:   s.lastFallbackError, fallbackErrorAt: s.fallbackErrorAt,
	}
}
