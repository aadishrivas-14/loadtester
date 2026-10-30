// Package gitconfig_test verifies that this repository's local git
// configuration matches the required identity and signing settings. These
// tests guard against accidental drift (e.g. a global config override or an
// unsigned-commit fallback) and run as part of `go test ./...`.
package gitconfig_test

import (
	"os/exec"
	"strings"
	"testing"
)

// gitConfig reads a single --local git config value for this repository.
// It returns the value and whether the key was set.
func gitConfig(t *testing.T, key string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 means the key is simply absent; anything else is a
		// real failure (e.g. git missing, not a repo).
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return "", false
		}
		t.Fatalf("git config --local --get %s: %v", key, err)
	}
	return strings.TrimSpace(string(out)), true
}

func requireGitRepo(t *testing.T) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not inside a git work tree; skipping git config checks")
	}
}

func TestGitUserName(t *testing.T) {
	requireGitRepo(t)
	const want = "aadishrivas-14"
	got, ok := gitConfig(t, "user.name")
	if !ok {
		t.Fatalf("local user.name is not set; want %q", want)
	}
	if got != want {
		t.Errorf("local user.name = %q, want %q", got, want)
	}
}

func TestGitUserEmail(t *testing.T) {
	requireGitRepo(t)
	const want = "aadishrivas@gmail.com"
	got, ok := gitConfig(t, "user.email")
	if !ok {
		t.Fatalf("local user.email is not set; want %q", want)
	}
	if got != want {
		t.Errorf("local user.email = %q, want %q", got, want)
	}
}

func TestGitCommitGPGSign(t *testing.T) {
	requireGitRepo(t)
	const want = "false"
	got, ok := gitConfig(t, "commit.gpgsign")
	if !ok {
		t.Fatalf("local commit.gpgsign is not set; want %q", want)
	}
	if got != want {
		t.Errorf("local commit.gpgsign = %q, want %q", got, want)
	}
}
