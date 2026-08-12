// Package loadtest is a lightweight, dependency-free HTTP load testing engine.
//
// It can be used as a library (import this package and call Run) or driven by
// the companion CLI in cmd/loadtest. It uses only the Go standard library.
package loadtest

import (
	"errors"
	"net/http"
	"time"
)

// Config describes a load test. Create one directly or via NewConfig, then
// pass it to Run. Zero values fall back to the defaults applied by normalize.
type Config struct {
	// URL is the target endpoint. Required.
	URL string

	// Method is the HTTP method (GET, POST, ...). Defaults to GET.
	Method string

	// Header holds request headers applied to every request.
	Header http.Header

	// Body is the raw request body sent with every request (optional).
	Body []byte

	// Concurrency is the number of parallel workers. Defaults to 10.
	Concurrency int

	// Requests is the total number of requests to send. If 0, the test runs
	// for Duration instead. One of Requests or Duration must be set.
	Requests int

	// Duration caps how long the test runs. If 0, the test stops after
	// Requests have been sent. Ignored when Requests > 0 and Duration == 0.
	Duration time.Duration

	// RatePerSecond optionally throttles the global request rate across all
	// workers. 0 means unlimited (as fast as possible).
	RatePerSecond float64

	// Profile selects the load shape over time. Defaults to ProfileConstant.
	Profile Profile

	// RampStart and RampEnd define the concurrency range for ProfileRamp.
	// Workers scale linearly from RampStart to RampEnd over Duration.
	// If unset, RampStart defaults to 1 and RampEnd to Concurrency.
	RampStart int
	RampEnd   int

	// Spike settings for ProfileSpike: a baseline load with periodic bursts.
	// SpikeConcurrency workers are active during each burst; the baseline uses
	// Concurrency workers. SpikeEvery is the burst period, SpikeFor its length.
	SpikeConcurrency int
	SpikeEvery       time.Duration
	SpikeFor         time.Duration

	// WarmUp excludes requests completed within this initial window from the
	// final statistics, so cold-start effects do not skew steady-state SLOs.
	WarmUp time.Duration

	// Interval, if > 0, emits live metrics snapshots at this cadence via the
	// OnInterval callback (and the CLI prints them).
	Interval time.Duration

	// OnInterval is invoked with a snapshot at each Interval tick (optional).
	OnInterval func(IntervalStats)

	// SLO defines pass/fail thresholds evaluated against the final Result.
	SLO *SLO

	// Timeout is the per-request timeout. Defaults to 30s.
	Timeout time.Duration

	// DisableKeepAlives forces a new connection per request when true.
	DisableKeepAlives bool

	// InsecureSkipVerify disables TLS certificate verification when true.
	InsecureSkipVerify bool
}

// Profile describes how offered load changes over the course of a test.
type Profile string

const (
	// ProfileConstant holds Concurrency workers for the whole run.
	ProfileConstant Profile = "constant"
	// ProfileRamp linearly scales workers from RampStart to RampEnd over
	// Duration, surfacing the throughput "knee" and latency degradation point.
	ProfileRamp Profile = "ramp"
	// ProfileSpike runs a baseline load with periodic concurrency bursts,
	// testing backpressure and recovery from sudden surges.
	ProfileSpike Profile = "spike"
)

// SLO holds service-level objective thresholds. A zero value for a field means
// that dimension is not checked.
type SLO struct {
	MaxP99       time.Duration // fail if p99 latency exceeds this
	MaxP95       time.Duration // fail if p95 latency exceeds this
	MaxErrorRate float64       // fail if error rate (0-1) exceeds this
	MinRPS       float64       // fail if throughput falls below this
}

// NewConfig returns a Config for the given URL with defaults applied.
func NewConfig(url string) *Config {
	c := &Config{URL: url}
	c.normalize()
	return c
}

// Validate checks that the configuration is runnable.
func (c *Config) Validate() error {
	if c.URL == "" {
		return errors.New("loadtest: URL is required")
	}
	if c.Requests <= 0 && c.Duration <= 0 {
		return errors.New("loadtest: either Requests or Duration must be set")
	}
	if c.Concurrency < 0 {
		return errors.New("loadtest: Concurrency must not be negative")
	}
	if c.RatePerSecond < 0 {
		return errors.New("loadtest: RatePerSecond must not be negative")
	}
	switch c.Profile {
	case "", ProfileConstant:
		// ok
	case ProfileRamp, ProfileSpike:
		if c.Duration <= 0 {
			return errors.New("loadtest: " + string(c.Profile) + " profile requires Duration")
		}
	default:
		return errors.New("loadtest: unknown profile " + string(c.Profile))
	}
	return nil
}

// normalize fills in default values for unset fields.
func (c *Config) normalize() {
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 10
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Header == nil {
		c.Header = make(http.Header)
	}
	if c.Profile == "" {
		c.Profile = ProfileConstant
	}
	if c.Profile == ProfileRamp {
		if c.RampStart <= 0 {
			c.RampStart = 1
		}
		if c.RampEnd <= 0 {
			c.RampEnd = c.Concurrency
		}
	}
	if c.Profile == ProfileSpike {
		if c.SpikeConcurrency <= 0 {
			c.SpikeConcurrency = c.Concurrency * 5
		}
		if c.SpikeEvery <= 0 {
			c.SpikeEvery = 10 * time.Second
		}
		if c.SpikeFor <= 0 {
			c.SpikeFor = time.Second
		}
	}
}
