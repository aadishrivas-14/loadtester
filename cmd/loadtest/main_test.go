package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildBinary compiles the CLI once per test binary run and returns its path.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "loadtest-cli")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "loadtest")
		// Build the current package (the CLI) into binPath.
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = &buildFailure{out: string(out), err: err}
		}
	})
	if buildErr != nil {
		t.Fatalf("building CLI: %v", buildErr)
	}
	return binPath
}

type buildFailure struct {
	out string
	err error
}

func (b *buildFailure) Error() string { return b.err.Error() + "\n" + b.out }

// run executes the built binary and returns stdout, stderr, and exit code.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running CLI: %v", err)
		}
	}
	return stdout.String(), stderr.String(), code
}

func okServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCLI_MissingURL(t *testing.T) {
	_, stderr, code := run(t)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "missing target URL") {
		t.Errorf("stderr missing usage error: %q", stderr)
	}
}

func TestCLI_NoCountOrDuration(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	_, stderr, code := run(t, srv.URL)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-n") || !strings.Contains(stderr, "-d") {
		t.Errorf("stderr should mention -n/-d: %q", stderr)
	}
}

func TestCLI_FixedRequests(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	stdout, _, code := run(t, "-n", "50", "-c", "5", srv.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "50 total, 50 ok") {
		t.Errorf("stdout missing expected summary: %q", stdout)
	}
}

func TestCLI_JSONOutput(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	stdout, _, code := run(t, "-n", "30", "-c", "3", "-json", srv.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if m["total_requests"].(float64) != 30 {
		t.Errorf("total_requests = %v, want 30", m["total_requests"])
	}
}

func TestCLI_SLOFailExitCode(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	// Impossible p99 target -> SLO must fail -> exit 1.
	stdout, _, code := run(t, "-n", "100", "-c", "10", "-slo-p99", "1ns", srv.URL)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (SLO fail)", code)
	}
	if !strings.Contains(stdout, "SLO:         FAIL") {
		t.Errorf("stdout should report SLO FAIL: %q", stdout)
	}
}

func TestCLI_SLOPassExitCode(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	// Generous SLO -> pass -> exit 0.
	stdout, _, code := run(t, "-n", "100", "-c", "10", "-slo-p99", "60s", srv.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (SLO pass)", code)
	}
	if !strings.Contains(stdout, "SLO:         PASS") {
		t.Errorf("stdout should report SLO PASS: %q", stdout)
	}
}

func TestCLI_RampProfile(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	stdout, stderr, code := run(t, "-profile", "ramp", "-d", "400ms",
		"-ramp-start", "2", "-ramp-end", "8", srv.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "ramp profile") {
		t.Errorf("stderr should describe ramp plan: %q", stderr)
	}
	if !strings.Contains(stdout, "total") {
		t.Errorf("stdout should contain a summary: %q", stdout)
	}
}

func TestCLI_UnknownProfile(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	_, stderr, code := run(t, "-profile", "bogus", "-n", "5", srv.URL)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for unknown profile", code)
	}
	if !strings.Contains(stderr, "unknown profile") {
		t.Errorf("stderr should mention unknown profile: %q", stderr)
	}
}

func TestCLI_BadHeader(t *testing.T) {
	srv := okServer()
	defer srv.Close()
	// Header without a colon is rejected by flag parsing (exit 2).
	_, stderr, code := run(t, "-H", "NoColonHere", "-n", "1", srv.URL)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for bad header", code)
	}
	if !strings.Contains(stderr, "invalid header") {
		t.Errorf("stderr should mention invalid header: %q", stderr)
	}
}

func TestCLI_PostWithHeaderReachesServer(t *testing.T) {
	var gotMethod, gotCT string
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			gotMethod = r.Method
			gotCT = r.Header.Get("Content-Type")
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, code := run(t, "-n", "5", "-c", "1", "-m", "POST",
		"-H", "Content-Type: application/json", "-body", `{"a":1}`, srv.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != "POST" {
		t.Errorf("server saw method %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("server saw Content-Type %q, want application/json", gotCT)
	}
}
