package loadtest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRun_FixedRequests(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, Requests: 200, Concurrency: 10}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if res.TotalRequests != 200 {
		t.Errorf("TotalRequests = %d, want 200", res.TotalRequests)
	}
	if res.SuccessCount != 200 {
		t.Errorf("SuccessCount = %d, want 200", res.SuccessCount)
	}
	if got := atomic.LoadInt64(&hits); got != 200 {
		t.Errorf("server received %d hits, want 200", got)
	}
	if res.StatusCodes[200] != 200 {
		t.Errorf("StatusCodes[200] = %d, want 200", res.StatusCodes[200])
	}
	if res.P50 <= 0 || res.P999 <= 0 {
		t.Errorf("expected positive p50/p999, got p50=%v p999=%v", res.P50, res.P999)
	}
}

func TestRun_Duration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, Duration: 300 * time.Millisecond, Concurrency: 5}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.TotalRequests == 0 {
		t.Error("expected some requests during duration run, got 0")
	}
	if res.Elapsed < 250*time.Millisecond {
		t.Errorf("elapsed too short: %v", res.Elapsed)
	}
}

func TestRun_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, Duration: 400 * time.Millisecond, Concurrency: 10, RatePerSecond: 50}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.TotalRequests > 40 {
		t.Errorf("rate limiting ineffective: %d requests in 400ms at 50 req/s", res.TotalRequests)
	}
}

func TestValidate(t *testing.T) {
	if err := (&Config{}).Validate(); err == nil {
		t.Error("expected error for empty config")
	}
	if err := (&Config{URL: "http://x"}).Validate(); err == nil {
		t.Error("expected error when neither Requests nor Duration set")
	}
	// Ramp without duration should fail.
	rc := &Config{URL: "http://x", Requests: 10, Profile: ProfileRamp}
	rc.normalize()
	if err := rc.Validate(); err == nil {
		t.Error("expected error for ramp profile without duration")
	}
}

func TestRun_WarmUpExcluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, Duration: 500 * time.Millisecond, Concurrency: 5, WarmUp: 200 * time.Millisecond}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.WarmUpExcluded == 0 {
		t.Error("expected some requests excluded by warm-up, got 0")
	}
	if res.TotalRequests == 0 {
		t.Error("expected post-warm-up requests, got 0")
	}
}

func TestRun_Ramp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		URL: srv.URL, Duration: 400 * time.Millisecond,
		Profile: ProfileRamp, RampStart: 1, RampEnd: 8,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.TotalRequests == 0 {
		t.Error("ramp produced no requests")
	}
}

func TestRun_Spike(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		URL: srv.URL, Duration: 500 * time.Millisecond, Concurrency: 2,
		Profile: ProfileSpike, SpikeConcurrency: 20,
		SpikeEvery: 150 * time.Millisecond, SpikeFor: 80 * time.Millisecond,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.TotalRequests == 0 {
		t.Error("spike produced no requests")
	}
}

func TestRun_Interval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var ticks int64
	cfg := &Config{
		URL: srv.URL, Duration: 450 * time.Millisecond, Concurrency: 5,
		Interval:   100 * time.Millisecond,
		OnInterval: func(IntervalStats) { atomic.AddInt64(&ticks, 1) },
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if atomic.LoadInt64(&ticks) < 2 {
		t.Errorf("expected multiple interval ticks, got %d", ticks)
	}
	if len(res.Intervals) < 2 {
		t.Errorf("expected multiple interval snapshots, got %d", len(res.Intervals))
	}
}

func TestSLO_Evaluate(t *testing.T) {
	res := &Result{
		TotalRequests: 100, SuccessCount: 90, ErrorCount: 10,
		Elapsed: time.Second,
		P95:     80 * time.Millisecond,
		P99:     500 * time.Millisecond,
	}

	// p99 of 500ms should fail a 250ms SLO; error rate 10% should fail 1%.
	rep := res.Evaluate(&SLO{MaxP99: 250 * time.Millisecond, MaxErrorRate: 0.01})
	if rep.Passed {
		t.Error("expected SLO to fail")
	}
	if len(rep.Failures) != 2 {
		t.Errorf("expected 2 failures, got %d: %v", len(rep.Failures), rep.Failures)
	}

	// Loose SLO should pass.
	res2 := &Result{TotalRequests: 100, SuccessCount: 100, Elapsed: time.Second, P99: 10 * time.Millisecond}
	rep2 := res2.Evaluate(&SLO{MaxP99: time.Second, MaxErrorRate: 0.5, MinRPS: 10})
	if !rep2.Passed {
		t.Errorf("expected SLO to pass, failures: %v", rep2.Failures)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrNone},
		{"canceled", context.Canceled, ErrCanceled},
		{"deadline", context.DeadlineExceeded, ErrTimeout},
		{"refused", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), ErrConnRefused},
		{"reset", errors.New("read tcp: connection reset by peer"), ErrConnReset},
		{"nohost", errors.New("dial tcp: lookup bad.invalid: no such host"), ErrDNS},
		{"tls", errors.New("tls: handshake failure"), ErrTLS},
		{"x509", errors.New("x509: certificate signed by unknown authority"), ErrTLS},
		{"other", errors.New("something weird"), ErrOther},
	}
	for _, c := range cases {
		if got := classifyError(c.err); got != c.want {
			t.Errorf("%s: classifyError = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClassifyError_NetTimeout(t *testing.T) {
	var e net.Error = &net.OpError{Op: "dial", Err: timeoutErr{}}
	if got := classifyError(e); got != ErrTimeout {
		t.Errorf("net timeout classified as %q, want timeout", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestResult_JSON(t *testing.T) {
	res := &Result{
		TotalRequests: 10, SuccessCount: 9, ErrorCount: 1,
		StatusCodes: map[int]int{200: 9},
		Errors:      map[ErrorClass]int{ErrTimeout: 1},
		Elapsed:     time.Second,
		P99:         5 * time.Millisecond,
	}
	data, err := res.JSON()
	if err != nil {
		t.Fatalf("JSON error: %v", err)
	}
	s := string(data)
	for _, want := range []string{"\"total_requests\": 10", "\"timeout\": 1", "\"p99\":"} {
		if !contains(s, want) {
			t.Errorf("JSON output missing %q\n%s", want, s)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
