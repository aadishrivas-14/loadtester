package loadtest

import (
	"encoding/json"
	"time"
)

// jsonResult is the JSON-friendly projection of a Result. Durations are
// emitted as milliseconds (floating point) for easy consumption by dashboards.
type jsonResult struct {
	TotalRequests  int            `json:"total_requests"`
	SuccessCount   int            `json:"success_count"`
	ErrorCount     int            `json:"error_count"`
	WarmUpExcluded int            `json:"warmup_excluded"`
	ErrorRate      float64        `json:"error_rate"`
	RPS            float64        `json:"rps"`
	ElapsedMS      float64        `json:"elapsed_ms"`
	Latency        jsonLatency    `json:"latency_ms"`
	StatusCodes    map[string]int `json:"status_codes"`
	Errors         map[string]int `json:"errors"`
	Intervals      []jsonInterval `json:"intervals,omitempty"`
	SLO            *jsonSLO       `json:"slo,omitempty"`
}

type jsonLatency struct {
	Min    float64 `json:"min"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	Max    float64 `json:"max"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	P999   float64 `json:"p999"`
}

type jsonInterval struct {
	ElapsedMS float64 `json:"elapsed_ms"`
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	RPS       float64 `json:"rps"`
	P99MS     float64 `json:"p99_ms"`
}

type jsonSLO struct {
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// JSON renders the Result as indented JSON.
func (r *Result) JSON() ([]byte, error) {
	jr := jsonResult{
		TotalRequests:  r.TotalRequests,
		SuccessCount:   r.SuccessCount,
		ErrorCount:     r.ErrorCount,
		WarmUpExcluded: r.WarmUpExcluded,
		ErrorRate:      r.ErrorRate(),
		RPS:            r.RequestsPerSecond(),
		ElapsedMS:      ms(r.Elapsed),
		Latency: jsonLatency{
			Min:    ms(r.Min),
			Mean:   ms(r.Mean),
			StdDev: ms(r.StdDev),
			Max:    ms(r.Max),
			P50:    ms(r.P50),
			P90:    ms(r.P90),
			P95:    ms(r.P95),
			P99:    ms(r.P99),
			P999:   ms(r.P999),
		},
		StatusCodes: make(map[string]int, len(r.StatusCodes)),
		Errors:      make(map[string]int, len(r.Errors)),
	}

	for code, n := range r.StatusCodes {
		jr.StatusCodes[itoa(code)] = n
	}
	for class, n := range r.Errors {
		jr.Errors[string(class)] = n
	}
	for _, iv := range r.Intervals {
		jr.Intervals = append(jr.Intervals, jsonInterval{
			ElapsedMS: ms(iv.Elapsed),
			Requests:  iv.Requests,
			Errors:    iv.Errors,
			RPS:       iv.RPS,
			P99MS:     ms(iv.P99),
		})
	}
	if r.SLO != nil {
		jr.SLO = &jsonSLO{Passed: r.SLO.Passed, Failures: r.SLO.Failures}
	}

	return json.MarshalIndent(jr, "", "  ")
}

// itoa is a tiny int->string helper to avoid importing strconv solely here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
