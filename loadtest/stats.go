package loadtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// sample is a single recorded request outcome kept for post-run analysis
// (warm-up filtering needs the completion timestamp, so we retain samples).
type sample struct {
	at      time.Duration // time since test start when the request completed
	latency time.Duration
	status  int
	class   ErrorClass
}

// Result is the aggregated outcome of a load test. It is safe to inspect after
// Run returns, which makes it convenient to assert on inside automated tests.
type Result struct {
	// TotalRequests is the number of requests that completed (success + error),
	// after any warm-up window has been excluded.
	TotalRequests int
	// SuccessCount is the number of requests with a 2xx/3xx status and no error.
	SuccessCount int
	// ErrorCount is the number of transport errors or non-2xx/3xx responses.
	ErrorCount int
	// WarmUpExcluded is the number of requests dropped by the warm-up filter.
	WarmUpExcluded int

	// StatusCodes maps HTTP status code -> count.
	StatusCodes map[int]int
	// Errors maps error class -> count (classified transport failures).
	Errors map[ErrorClass]int

	// Elapsed is the wall-clock duration of the test run.
	Elapsed time.Duration

	// Derived latency metrics (over post-warm-up, successful+error samples).
	Min, Max, Mean, StdDev   time.Duration
	P50, P90, P95, P99, P999 time.Duration

	// Intervals holds per-interval snapshots when Config.Interval was set.
	Intervals []IntervalStats

	// SLO holds the pass/fail evaluation when Config.SLO was set.
	SLO *SLOReport
}

// ErrorRate returns the fraction of requests that failed (0-1).
func (r *Result) ErrorRate() float64 {
	if r.TotalRequests == 0 {
		return 0
	}
	return float64(r.ErrorCount) / float64(r.TotalRequests)
}

// RequestsPerSecond returns throughput over the elapsed wall-clock time.
func (r *Result) RequestsPerSecond() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.TotalRequests) / r.Elapsed.Seconds()
}

// IntervalStats is a snapshot of activity over one reporting interval.
type IntervalStats struct {
	Elapsed  time.Duration // time since start at end of interval
	Requests int           // requests completed during the interval
	Errors   int           // errors during the interval
	RPS      float64       // throughput during the interval
	P99      time.Duration // p99 latency during the interval
}

// SLOReport is the outcome of evaluating a Result against an SLO.
type SLOReport struct {
	Passed   bool
	Failures []string
}

// collector accumulates per-request samples behind a mutex. It supports both
// live interval snapshots and a final warm-up-filtered summary.
type collector struct {
	mu      sync.Mutex
	samples []sample
	warmUp  time.Duration
	// cursor for interval snapshots (index into samples already reported).
	intervalCursor int
}

func newCollector(warmUp time.Duration) *collector {
	return &collector{
		samples: make([]sample, 0, 4096),
		warmUp:  warmUp,
	}
}

// record ingests a single request outcome.
func (c *collector) record(at, latency time.Duration, status int, class ErrorClass) {
	c.mu.Lock()
	c.samples = append(c.samples, sample{at: at, latency: latency, status: status, class: class})
	c.mu.Unlock()
}

// snapshot produces an IntervalStats over samples recorded since the previous
// snapshot. It is used for live reporting and does not apply warm-up filtering.
func (c *collector) snapshot(elapsed time.Duration) IntervalStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	window := c.samples[c.intervalCursor:]
	c.intervalCursor = len(c.samples)

	st := IntervalStats{Elapsed: elapsed, Requests: len(window)}
	if len(window) == 0 {
		return st
	}

	lat := make([]time.Duration, 0, len(window))
	for _, s := range window {
		if s.class != ErrNone || !isSuccessStatus(s.status) {
			st.Errors++
		}
		lat = append(lat, s.latency)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	st.P99 = percentile(lat, 99)

	// RPS for this window is approximated against its own share of elapsed time.
	// We compute it against the interval length in the runner instead; here we
	// leave RPS to be filled by the caller which knows the interval width.
	return st
}

// finalize applies warm-up filtering, computes summary metrics, and returns
// the Result.
func (c *collector) finalize(elapsed time.Duration) Result {
	c.mu.Lock()
	defer c.mu.Unlock()

	r := Result{
		StatusCodes: make(map[int]int),
		Errors:      make(map[ErrorClass]int),
		Elapsed:     elapsed,
	}

	lat := make([]time.Duration, 0, len(c.samples))
	for _, s := range c.samples {
		if s.at < c.warmUp {
			r.WarmUpExcluded++
			continue
		}
		r.TotalRequests++
		lat = append(lat, s.latency)

		if s.class != ErrNone {
			r.ErrorCount++
			r.Errors[s.class]++
			continue
		}
		r.StatusCodes[s.status]++
		if isSuccessStatus(s.status) {
			r.SuccessCount++
		} else {
			r.ErrorCount++
		}
	}

	if len(lat) == 0 {
		return r
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	r.Min = lat[0]
	r.Max = lat[len(lat)-1]
	r.Mean = meanDuration(lat)
	r.StdDev = stdDevDuration(lat, r.Mean)
	r.P50 = percentile(lat, 50)
	r.P90 = percentile(lat, 90)
	r.P95 = percentile(lat, 95)
	r.P99 = percentile(lat, 99)
	r.P999 = percentile(lat, 99.9)

	return r
}

func isSuccessStatus(status int) bool {
	return status >= 200 && status < 400
}

// Evaluate checks the Result against an SLO and records the report on it.
func (r *Result) Evaluate(slo *SLO) *SLOReport {
	if slo == nil {
		return nil
	}
	rep := &SLOReport{Passed: true}

	if slo.MaxP99 > 0 && r.P99 > slo.MaxP99 {
		rep.Passed = false
		rep.Failures = append(rep.Failures,
			fmt.Sprintf("p99 %s exceeds max %s", round(r.P99), slo.MaxP99))
	}
	if slo.MaxP95 > 0 && r.P95 > slo.MaxP95 {
		rep.Passed = false
		rep.Failures = append(rep.Failures,
			fmt.Sprintf("p95 %s exceeds max %s", round(r.P95), slo.MaxP95))
	}
	if slo.MaxErrorRate > 0 && r.ErrorRate() > slo.MaxErrorRate {
		rep.Passed = false
		rep.Failures = append(rep.Failures,
			fmt.Sprintf("error rate %.2f%% exceeds max %.2f%%",
				r.ErrorRate()*100, slo.MaxErrorRate*100))
	}
	if slo.MinRPS > 0 && r.RequestsPerSecond() < slo.MinRPS {
		rep.Passed = false
		rep.Failures = append(rep.Failures,
			fmt.Sprintf("throughput %.1f req/s below min %.1f req/s",
				r.RequestsPerSecond(), slo.MinRPS))
	}

	r.SLO = rep
	return rep
}

// percentile returns the p-th percentile (0-100) from a sorted slice using
// nearest-rank. The slice must already be sorted ascending.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int((p/100)*float64(len(sorted)) + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func meanDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

func stdDevDuration(ds []time.Duration, mean time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	var sumSq float64
	m := float64(mean)
	for _, d := range ds {
		diff := float64(d) - m
		sumSq += diff * diff
	}
	return time.Duration(math.Sqrt(sumSq / float64(len(ds))))
}

// String renders a human-readable summary of the result.
func (r *Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Requests:    %d total, %d ok, %d failed", r.TotalRequests, r.SuccessCount, r.ErrorCount)
	if r.WarmUpExcluded > 0 {
		fmt.Fprintf(&b, " (%d excluded as warm-up)", r.WarmUpExcluded)
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "Duration:    %s\n", r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "Throughput:  %.2f req/s\n", r.RequestsPerSecond())
	fmt.Fprintf(&b, "Error rate:  %.2f%%\n", r.ErrorRate()*100)
	fmt.Fprintf(&b, "Latency:     min %s | mean %s | stddev %s | max %s\n",
		round(r.Min), round(r.Mean), round(r.StdDev), round(r.Max))
	fmt.Fprintf(&b, "Percentiles: p50 %s | p90 %s | p95 %s | p99 %s | p99.9 %s\n",
		round(r.P50), round(r.P90), round(r.P95), round(r.P99), round(r.P999))

	if len(r.StatusCodes) > 0 {
		codes := make([]int, 0, len(r.StatusCodes))
		for c := range r.StatusCodes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		b.WriteString("Status codes:\n")
		for _, c := range codes {
			fmt.Fprintf(&b, "  [%d] %d\n", c, r.StatusCodes[c])
		}
	}

	if len(r.Errors) > 0 {
		classes := make([]string, 0, len(r.Errors))
		for c := range r.Errors {
			classes = append(classes, string(c))
		}
		sort.Strings(classes)
		b.WriteString("Error classes:\n")
		for _, c := range classes {
			fmt.Fprintf(&b, "  %-14s %d\n", c, r.Errors[ErrorClass(c)])
		}
	}

	if r.SLO != nil {
		if r.SLO.Passed {
			b.WriteString("SLO:         PASS\n")
		} else {
			b.WriteString("SLO:         FAIL\n")
			for _, f := range r.SLO.Failures {
				fmt.Fprintf(&b, "  - %s\n", f)
			}
		}
	}

	return b.String()
}

func round(d time.Duration) time.Duration {
	return d.Round(time.Microsecond)
}
