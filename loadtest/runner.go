package loadtest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"sync"
	"time"
)

// Run executes the load test described by cfg and blocks until it finishes,
// the context is cancelled, or the configured Duration/Requests limit is hit.
//
// It returns an aggregated Result. A non-nil error is only returned for
// configuration problems; per-request failures are captured in Result.
func Run(ctx context.Context, cfg *Config) (Result, error) {
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}

	client := newClient(cfg)
	defer client.CloseIdleConnections()

	coll := newCollector(cfg.WarmUp)

	// Apply an overall duration cap if requested.
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	start := time.Now()

	// Optional live interval reporting.
	var intervalWG sync.WaitGroup
	var intervals []IntervalStats
	var intervalsMu sync.Mutex
	stopIntervals := make(chan struct{})
	if cfg.Interval > 0 {
		intervalWG.Add(1)
		go func() {
			defer intervalWG.Done()
			ticker := time.NewTicker(cfg.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-stopIntervals:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					snap := coll.snapshot(time.Since(start))
					snap.RPS = float64(snap.Requests) / cfg.Interval.Seconds()
					intervalsMu.Lock()
					intervals = append(intervals, snap)
					intervalsMu.Unlock()
					if cfg.OnInterval != nil {
						cfg.OnInterval(snap)
					}
				}
			}
		}()
	}

	// Dispatch work according to the selected profile.
	switch cfg.Profile {
	case ProfileRamp:
		runRamp(ctx, cfg, client, coll, start)
	case ProfileSpike:
		runSpike(ctx, cfg, client, coll, start)
	default:
		runConstant(ctx, cfg, client, coll, start)
	}

	close(stopIntervals)
	intervalWG.Wait()

	result := coll.finalize(time.Since(start))
	intervalsMu.Lock()
	result.Intervals = intervals
	intervalsMu.Unlock()

	if cfg.SLO != nil {
		result.Evaluate(cfg.SLO)
	}
	return result, nil
}

// workerPool runs `workers` goroutines pulling from jobs until it is closed or
// ctx is done, applying the shared rate limiter if present.
func workerPool(ctx context.Context, workers int, jobs <-chan struct{}, limiter <-chan time.Time, client *http.Client, cfg *Config, coll *collector, start time.Time) {
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for range jobs {
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter:
					}
				}
				status, latency, err := doRequest(ctx, client, cfg)
				coll.record(time.Since(start), latency, status, classifyError(err))
			}
		}()
	}
	wg.Wait()
}

// newLimiter returns a rate-limiter channel (or nil) and a stop func.
func newLimiter(rate float64) (<-chan time.Time, func()) {
	if rate <= 0 {
		return nil, func() {}
	}
	interval := time.Duration(float64(time.Second) / rate)
	t := time.NewTicker(interval)
	return t.C, t.Stop
}

// runConstant: fixed worker count, either request-bounded or duration-bounded.
func runConstant(ctx context.Context, cfg *Config, client *http.Client, coll *collector, start time.Time) {
	limiter, stop := newLimiter(cfg.RatePerSecond)
	defer stop()

	jobs := make(chan struct{})
	go dispatch(ctx, cfg.Requests, jobs)
	workerPool(ctx, cfg.Concurrency, jobs, limiter, client, cfg, coll, start)
}

// runRamp: linearly scale active workers from RampStart to RampEnd over
// Duration. Implemented as a fleaky pool where new workers join over time.
func runRamp(ctx context.Context, cfg *Config, client *http.Client, coll *collector, start time.Time) {
	limiter, stop := newLimiter(cfg.RatePerSecond)
	defer stop()

	jobs := make(chan struct{})
	go dispatch(ctx, 0, jobs) // duration-bounded dispatch

	steps := cfg.RampEnd - cfg.RampStart
	if steps < 0 {
		steps = 0
	}
	stepDur := cfg.Duration
	if steps > 0 {
		stepDur = cfg.Duration / time.Duration(steps+1)
	}

	var wg sync.WaitGroup
	launch := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter:
					}
				}
				status, latency, err := doRequest(ctx, client, cfg)
				coll.record(time.Since(start), latency, status, classifyError(err))
			}
		}()
	}

	// Launch the starting workers immediately.
	for i := 0; i < cfg.RampStart; i++ {
		launch()
	}
	// Add one worker per step on a timer until RampEnd or context end.
	go func() {
		ticker := time.NewTicker(stepDur)
		defer ticker.Stop()
		added := 0
		for added < steps {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				launch()
				added++
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
}

// runSpike: a baseline worker pool plus periodic bursts of extra workers.
func runSpike(ctx context.Context, cfg *Config, client *http.Client, coll *collector, start time.Time) {
	limiter, stop := newLimiter(cfg.RatePerSecond)
	defer stop()

	jobs := make(chan struct{})
	go dispatch(ctx, 0, jobs)

	worker := func(stop <-chan struct{}) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-jobs:
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter:
					}
				}
				status, latency, err := doRequest(ctx, client, cfg)
				coll.record(time.Since(start), latency, status, classifyError(err))
			}
		}
	}

	var wg sync.WaitGroup
	// Baseline workers for the whole run.
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker(ctx.Done()) }()
	}

	// Spike controller: periodically launch a burst of extra workers.
	go func() {
		ticker := time.NewTicker(cfg.SpikeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				burstStop := make(chan struct{})
				extra := cfg.SpikeConcurrency - cfg.Concurrency
				for i := 0; i < extra; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); worker(burstStop) }()
				}
				// End the burst after SpikeFor.
				go func() {
					select {
					case <-ctx.Done():
					case <-time.After(cfg.SpikeFor):
					}
					close(burstStop)
				}()
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
}

// dispatch feeds the jobs channel. If total > 0 it enqueues exactly that many
// jobs; otherwise it feeds continuously until ctx is done. It closes jobs when
// finished so workers can drain and exit.
func dispatch(ctx context.Context, total int, jobs chan<- struct{}) {
	defer close(jobs)
	if total > 0 {
		for i := 0; i < total; i++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- struct{}{}:
			}
		}
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case jobs <- struct{}{}:
		}
	}
}

// newClient builds an http.Client tuned for load generation.
func newClient(cfg *Config) *http.Client {
	maxConns := cfg.Concurrency * 2
	if cfg.Profile == ProfileSpike && cfg.SpikeConcurrency > cfg.Concurrency {
		maxConns = cfg.SpikeConcurrency * 2
	}
	transport := &http.Transport{
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   cfg.DisableKeepAlives,
	}
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}
}

// doRequest performs a single HTTP request and returns its status, latency,
// and any transport error. The response body is drained and discarded so the
// connection can be reused.
func doRequest(ctx context.Context, client *http.Client, cfg *Config) (int, time.Duration, error) {
	var bodyReader io.Reader
	if len(cfg.Body) > 0 {
		bodyReader = bytes.NewReader(cfg.Body)
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bodyReader)
	if err != nil {
		return 0, 0, err
	}
	for k, vs := range cfg.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	reqStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(reqStart), err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, time.Since(reqStart), nil
}
