// Command loadtest is a lightweight HTTP load testing CLI built on the
// github.com/eshdiar/loadtester/loadtest library.
//
// It can surface common system-design failure modes: throughput saturation
// (ramp profile), spike/backpressure handling (spike profile), tail-latency
// degradation (p99/p99.9), error classification, and SLO pass/fail gating.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eshdiar/loadtester/loadtest"
)

// headerFlag collects repeated -H "Key: Value" flags.
type headerFlag struct {
	h http.Header
}

func (f *headerFlag) String() string { return "" }

func (f *headerFlag) Set(v string) error {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid header %q, expected 'Key: Value'", v)
	}
	if f.h == nil {
		f.h = make(http.Header)
	}
	f.h.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	return nil
}

func main() {
	var (
		requests = flag.Int("n", 0, "total number of requests (0 = use -d duration)")
		conc     = flag.Int("c", 10, "number of concurrent workers (baseline for spike)")
		duration = flag.Duration("d", 0, "test duration, e.g. 30s, 2m (used when -n is 0)")
		rate     = flag.Float64("rate", 0, "max requests per second across all workers (0 = unlimited)")
		method   = flag.String("m", "GET", "HTTP method")
		body     = flag.String("body", "", "request body")
		bodyFile = flag.String("body-file", "", "read request body from a file")
		timeout  = flag.Duration("timeout", 30*time.Second, "per-request timeout")
		noKeep   = flag.Bool("disable-keepalive", false, "disable HTTP keep-alive")
		insecure = flag.Bool("insecure", false, "skip TLS certificate verification")

		// Load profile.
		profile    = flag.String("profile", "constant", "load profile: constant | ramp | spike")
		rampStart  = flag.Int("ramp-start", 0, "ramp: starting worker count (default 1)")
		rampEnd    = flag.Int("ramp-end", 0, "ramp: ending worker count (default -c)")
		spikeConc  = flag.Int("spike-c", 0, "spike: worker count during a burst (default 5x -c)")
		spikeEvery = flag.Duration("spike-every", 0, "spike: period between bursts (default 10s)")
		spikeFor   = flag.Duration("spike-for", 0, "spike: burst length (default 1s)")

		// Analysis.
		warmUp   = flag.Duration("warmup", 0, "exclude requests in this initial window from stats")
		interval = flag.Duration("interval", 0, "print live metrics every interval, e.g. 1s")
		asJSON   = flag.Bool("json", false, "output the final result as JSON")

		// SLO gating.
		sloP99 = flag.Duration("slo-p99", 0, "SLO: fail if p99 latency exceeds this")
		sloP95 = flag.Duration("slo-p95", 0, "SLO: fail if p95 latency exceeds this")
		sloErr = flag.Float64("slo-error-rate", 0, "SLO: fail if error rate exceeds this (0-1)")
		sloRPS = flag.Float64("slo-min-rps", 0, "SLO: fail if throughput falls below this")
	)
	var headers headerFlag
	flag.Var(&headers, "H", "request header 'Key: Value' (repeatable)")

	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "error: missing target URL")
		flag.Usage()
		os.Exit(2)
	}
	url := flag.Arg(0)

	if *requests <= 0 && *duration <= 0 {
		fmt.Fprintln(os.Stderr, "error: specify -n (request count) or -d (duration)")
		os.Exit(2)
	}

	reqBody := []byte(*body)
	if *bodyFile != "" {
		data, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: reading body file: %v\n", err)
			os.Exit(1)
		}
		reqBody = data
	}

	var slo *loadtest.SLO
	if *sloP99 > 0 || *sloP95 > 0 || *sloErr > 0 || *sloRPS > 0 {
		slo = &loadtest.SLO{
			MaxP99:       *sloP99,
			MaxP95:       *sloP95,
			MaxErrorRate: *sloErr,
			MinRPS:       *sloRPS,
		}
	}

	cfg := &loadtest.Config{
		URL:                url,
		Method:             *method,
		Header:             headers.h,
		Body:               reqBody,
		Concurrency:        *conc,
		Requests:           *requests,
		Duration:           *duration,
		RatePerSecond:      *rate,
		Profile:            loadtest.Profile(*profile),
		RampStart:          *rampStart,
		RampEnd:            *rampEnd,
		SpikeConcurrency:   *spikeConc,
		SpikeEvery:         *spikeEvery,
		SpikeFor:           *spikeFor,
		WarmUp:             *warmUp,
		Interval:           *interval,
		SLO:                slo,
		Timeout:            *timeout,
		DisableKeepAlives:  *noKeep,
		InsecureSkipVerify: *insecure,
	}

	// Live interval printing to stderr so it doesn't pollute JSON on stdout.
	if *interval > 0 {
		cfg.OnInterval = func(s loadtest.IntervalStats) {
			fmt.Fprintf(os.Stderr, "[%6s] %5d req | %6.0f req/s | %4d err | p99 %s\n",
				s.Elapsed.Round(time.Millisecond), s.Requests, s.RPS, s.Errors,
				s.P99.Round(time.Microsecond))
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "Load testing %s\n  %s\n\n", url, describePlan(cfg))

	result, err := loadtest.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	if *asJSON {
		data, err := result.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: encoding JSON: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(string(data))
	} else {
		fmt.Print(result.String())
	}

	// Exit codes for CI gating:
	//   0 = ok, 1 = SLO failed or all requests failed, 2 = usage/config error.
	if result.SLO != nil && !result.SLO.Passed {
		os.Exit(1)
	}
	if result.TotalRequests > 0 && result.SuccessCount == 0 {
		os.Exit(1)
	}
}

func describePlan(cfg *loadtest.Config) string {
	var b strings.Builder
	switch loadtest.Profile(firstNonEmpty(string(cfg.Profile), "constant")) {
	case loadtest.ProfileRamp:
		fmt.Fprintf(&b, "ramp profile: workers %d->%d over %s", cfg.RampStart, cfg.RampEnd, cfg.Duration)
	case loadtest.ProfileSpike:
		fmt.Fprintf(&b, "spike profile: baseline %d, burst %d every %s for %s, over %s",
			cfg.Concurrency, cfg.SpikeConcurrency, cfg.SpikeEvery, cfg.SpikeFor, cfg.Duration)
	default:
		if cfg.Requests > 0 {
			fmt.Fprintf(&b, "constant: %d requests, %d workers", cfg.Requests, cfg.Concurrency)
		} else {
			fmt.Fprintf(&b, "constant: %s duration, %d workers", cfg.Duration, cfg.Concurrency)
		}
	}
	if cfg.RatePerSecond > 0 {
		fmt.Fprintf(&b, ", %.0f req/s cap", cfg.RatePerSecond)
	}
	if cfg.WarmUp > 0 {
		fmt.Fprintf(&b, ", %s warm-up", cfg.WarmUp)
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func usage() {
	fmt.Fprintf(os.Stderr, `loadtest - lightweight HTTP load tester for system-design testing

Usage:
  loadtest [flags] <url>

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Examples:
  # Basic throughput/latency
  loadtest -n 1000 -c 50 https://example.com

  # Find the saturation knee: ramp 1 -> 200 workers over 60s
  loadtest -profile ramp -d 60s -ramp-start 1 -ramp-end 200 -interval 5s https://example.com

  # Spike test: 10 baseline workers, bursts of 200 every 15s for 2s
  loadtest -profile spike -d 90s -c 10 -spike-c 200 -spike-every 15s -spike-for 2s https://example.com

  # CI gate with SLOs (exit 1 if violated), excluding 5s cold start
  loadtest -d 30s -c 100 -warmup 5s -slo-p99 250ms -slo-error-rate 0.01 -slo-min-rps 500 https://example.com

  # JSON output for dashboards
  loadtest -n 2000 -c 100 -json https://example.com
`)
}
