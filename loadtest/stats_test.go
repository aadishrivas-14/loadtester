package loadtest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPercentile_NearestRank(t *testing.T) {
	// 1..10 ms sorted.
	sorted := make([]time.Duration, 10)
	for i := 0; i < 10; i++ {
		sorted[i] = time.Duration(i+1) * time.Millisecond
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0, 1 * time.Millisecond},
		{50, 5 * time.Millisecond},
		{90, 9 * time.Millisecond},
		{99, 10 * time.Millisecond},
		{100, 10 * time.Millisecond},
	}
	for _, c := range cases {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("percentile(%.1f) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestPercentile_Empty(t *testing.T) {
	if got := percentile(nil, 99); got != 0 {
		t.Errorf("percentile of empty = %v, want 0", got)
	}
}

func TestPercentile_Single(t *testing.T) {
	s := []time.Duration{42 * time.Millisecond}
	for _, p := range []float64{0, 50, 99, 100} {
		if got := percentile(s, p); got != 42*time.Millisecond {
			t.Errorf("percentile(%.0f) of single = %v, want 42ms", p, got)
		}
	}
}

func TestMeanAndStdDev(t *testing.T) {
	ds := []time.Duration{2, 4, 4, 4, 5, 5, 7, 9} // classic stddev=2 dataset (mean=5)
	mean := meanDuration(ds)
	if mean != 5 {
		t.Errorf("mean = %v, want 5", mean)
	}
	if sd := stdDevDuration(ds, mean); sd != 2 {
		t.Errorf("stddev = %v, want 2", sd)
	}
}

func TestStdDev_LessThanTwo(t *testing.T) {
	if sd := stdDevDuration([]time.Duration{5}, 5); sd != 0 {
		t.Errorf("stddev of single = %v, want 0", sd)
	}
	if sd := stdDevDuration(nil, 0); sd != 0 {
		t.Errorf("stddev of empty = %v, want 0", sd)
	}
}

func TestConfig_NormalizeDefaults(t *testing.T) {
	c := &Config{URL: "http://x", Requests: 1}
	c.normalize()

	if c.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", c.Method)
	}
	if c.Concurrency != 10 {
		t.Errorf("Concurrency = %d, want 10", c.Concurrency)
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", c.Timeout)
	}
	if c.Header == nil {
		t.Error("Header should be initialized")
	}
	if c.Profile != ProfileConstant {
		t.Errorf("Profile = %q, want constant", c.Profile)
	}
}

func TestConfig_RampDefaults(t *testing.T) {
	c := &Config{URL: "http://x", Duration: time.Second, Concurrency: 50, Profile: ProfileRamp}
	c.normalize()
	if c.RampStart != 1 {
		t.Errorf("RampStart = %d, want 1", c.RampStart)
	}
	if c.RampEnd != 50 {
		t.Errorf("RampEnd = %d, want 50 (=Concurrency)", c.RampEnd)
	}
}

func TestConfig_SpikeDefaults(t *testing.T) {
	c := &Config{URL: "http://x", Duration: time.Second, Concurrency: 4, Profile: ProfileSpike}
	c.normalize()
	if c.SpikeConcurrency != 20 {
		t.Errorf("SpikeConcurrency = %d, want 20 (=5x Concurrency)", c.SpikeConcurrency)
	}
	if c.SpikeEvery != 10*time.Second {
		t.Errorf("SpikeEvery = %v, want 10s", c.SpikeEvery)
	}
	if c.SpikeFor != time.Second {
		t.Errorf("SpikeFor = %v, want 1s", c.SpikeFor)
	}
}

func TestConfig_UnknownProfile(t *testing.T) {
	c := &Config{URL: "http://x", Requests: 1, Profile: Profile("bogus")}
	c.normalize()
	if err := c.Validate(); err == nil {
		t.Error("expected error for unknown profile")
	}
}

func TestResult_ErrorRateAndRPS_ZeroSafe(t *testing.T) {
	r := &Result{}
	if r.ErrorRate() != 0 {
		t.Errorf("ErrorRate on empty = %v, want 0", r.ErrorRate())
	}
	if r.RequestsPerSecond() != 0 {
		t.Errorf("RPS on empty = %v, want 0", r.RequestsPerSecond())
	}
}

func TestSLO_BoundaryNotExceeded(t *testing.T) {
	// Exactly at the threshold should PASS (we only fail when strictly over).
	r := &Result{TotalRequests: 100, SuccessCount: 100, Elapsed: time.Second,
		P99: 100 * time.Millisecond}
	rep := r.Evaluate(&SLO{MaxP99: 100 * time.Millisecond, MinRPS: 100})
	if !rep.Passed {
		t.Errorf("expected PASS at exact boundary, failures: %v", rep.Failures)
	}
}

func TestSLO_Nil(t *testing.T) {
	r := &Result{}
	if rep := r.Evaluate(nil); rep != nil {
		t.Error("Evaluate(nil) should return nil")
	}
}

func TestResult_JSON_RoundTrip(t *testing.T) {
	r := &Result{
		TotalRequests: 50, SuccessCount: 48, ErrorCount: 2,
		StatusCodes: map[int]int{200: 48, 500: 2},
		Errors:      map[ErrorClass]int{ErrTimeout: 1, ErrConnRefused: 1},
		Elapsed:     2 * time.Second,
		P99:         12 * time.Millisecond,
		Intervals:   []IntervalStats{{Elapsed: time.Second, Requests: 25, RPS: 25, P99: 10 * time.Millisecond}},
		SLO:         &SLOReport{Passed: false, Failures: []string{"x"}},
	}
	data, err := r.JSON()
	if err != nil {
		t.Fatalf("JSON error: %v", err)
	}

	// Unmarshal into a generic map to confirm structure/values survive.
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("produced invalid JSON: %v", err)
	}
	if m["total_requests"].(float64) != 50 {
		t.Errorf("total_requests = %v, want 50", m["total_requests"])
	}
	codes := m["status_codes"].(map[string]interface{})
	if codes["200"].(float64) != 48 {
		t.Errorf("status_codes[200] = %v, want 48", codes["200"])
	}
	slo := m["slo"].(map[string]interface{})
	if slo["passed"].(bool) != false {
		t.Errorf("slo.passed = %v, want false", slo["passed"])
	}
	if len(m["intervals"].([]interface{})) != 1 {
		t.Errorf("expected 1 interval in JSON")
	}
}

func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 200: "200", -5: "-5", 404: "404"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}
