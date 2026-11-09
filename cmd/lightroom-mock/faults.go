package main

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// faultConfig is what the demo operator sets via POST /admin/faults. A POST
// replaces the whole config; {} heals everything.
type faultConfig struct {
	ErrorRate float64 `json:"error_rate"` // 0..1, share of API requests failed
	Status    int     `json:"status"`     // HTTP status for failed requests, default 503
	LatencyMS int     `json:"latency_ms"` // added to every API request before it is handled
}

const maxLatencyMS = 120_000

func (c *faultConfig) normalize() error {
	if c.Status == 0 {
		c.Status = 503
	}
	switch {
	case c.ErrorRate < 0 || c.ErrorRate > 1:
		return errors.New("error_rate must be between 0 and 1")
	case c.Status < 400 || c.Status > 599:
		return errors.New("status must be between 400 and 599")
	case c.LatencyMS < 0 || c.LatencyMS > maxLatencyMS:
		return errors.New("latency_ms must be between 0 and 120000")
	}
	return nil
}

// faults holds the current fault config. It is safe for concurrent use.
type faults struct {
	mu       sync.Mutex
	cfg      faultConfig
	injected int // requests failed by the injector since the last reset
}

func newFaults() *faults { return &faults{cfg: faultConfig{Status: 503}} }

func (f *faults) get() faultConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *faults) set(c faultConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = c
}

func (f *faults) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = faultConfig{Status: 503}
	f.injected = 0
}

// decide rolls the dice for one request. It returns the latency to add and,
// if the request must fail, the status to fail it with (0 = let it through).
// rand.Float64 is in [0,1), so error_rate 1 always fails and 0 never does.
func (f *faults) decide() (latency time.Duration, failStatus int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	latency = time.Duration(f.cfg.LatencyMS) * time.Millisecond
	if rand.Float64() < f.cfg.ErrorRate {
		f.injected++
		return latency, f.cfg.Status
	}
	return latency, 0
}

func (f *faults) injectedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected
}
