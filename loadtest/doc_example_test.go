package loadtest_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/eshdiar/loadtester/loadtest"
)

// Example demonstrates running a small load test against a server and reading
// the aggregated Result. It is also executed as a test (the Output comment is
// verified by `go test`).
func Example() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &loadtest.Config{
		URL:         srv.URL,
		Requests:    100,
		Concurrency: 10,
		Timeout:     5 * time.Second,
	}

	res, err := loadtest.Run(context.Background(), cfg)
	if err != nil {
		panic(err)
	}

	fmt.Println("total:", res.TotalRequests)
	fmt.Println("ok:", res.SuccessCount)
	fmt.Println("errors:", res.ErrorCount)

	// Output:
	// total: 100
	// ok: 100
	// errors: 0
}

// ExampleResult_Evaluate shows gating a Result against an SLO.
func ExampleResult_Evaluate() {
	res := &loadtest.Result{
		TotalRequests: 1000,
		SuccessCount:  1000,
		Elapsed:       time.Second,
		P99:           20 * time.Millisecond,
	}
	rep := res.Evaluate(&loadtest.SLO{MaxP99: 50 * time.Millisecond})
	fmt.Println("passed:", rep.Passed)

	// Output:
	// passed: true
}
