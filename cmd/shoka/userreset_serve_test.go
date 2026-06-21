package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// These cover the B-28 case-2 main()-level properties that the in-process unit tests
// cannot: the `--reset-password` startup mode EXITS WITHOUT SERVING (no auth-disabled
// window — if it served, the process would not exit and the test times out), and there
// is NO `--password` flag (the new password is TTY/stdin-only, never argv).

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// shokaBinary builds the server binary once per test process and returns its path.
func shokaBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		builtBin = filepath.Join(os.TempDir(), "shoka-userreset-test-bin")
		cmd := exec.Command("go", "build", "-o", builtBin, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("build output: %s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build shoka binary: %v", buildErr)
	}
	return builtBin
}

func writeResetConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := strings.Join([]string{
		"server:",
		"  http:",
		`    listen: "127.0.0.1:0"`,
		"  mcp:",
		"    plain:",
		`      listen: "127.0.0.1:0"`,
		"  auth:",
		"    enabled: false",
		"  log:",
		`    level: "error"`,
		"storage:",
		`  base_dir: "` + filepath.Join(dir, "data") + `"`,
		"",
	}, "\n")
	p := filepath.Join(dir, "shoka.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// seedUser creates the userstore (under base_dir) with one admin, then closes it so the
// subprocess can acquire the bbolt lock.
func seedUser(t *testing.T, dir, email, password string) {
	t.Helper()
	base := filepath.Join(dir, "data")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	key, err := userstore.ResolveTOTPKey("", filepath.Join(base, "userstore.key"))
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	s, err := userstore.Open(filepath.Join(base, "users.db"), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ph, _ := userstore.HashPassword(password)
	if err := s.CreateFirstAdmin(&userstore.UserRecord{Email: email, PasswordHash: ph}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	_ = s.Close()
}

// TestStartupReset_ExitsWithoutServing: `shoka --reset-password <email>` resets the
// password and EXITS (within a short deadline) — it does not serve. If it fell through
// to serving (a MySQL --skip-grant-tables-style auth window), the process would stay up
// and Wait would block past the deadline → the test fails.
func TestStartupReset_ExitsWithoutServing(t *testing.T) {
	bin := shokaBinary(t)
	dir := t.TempDir()
	cfgPath := writeResetConfig(t, dir)
	seedUser(t, dir, "admin@example.com", "oldpassword1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath, "--reset-password", "admin@example.com")
	cmd.Stdin = strings.NewReader("newpassword1\nnewpassword1\n")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("the reset mode did NOT exit — it appears to have served (output: %s)", out)
	}
	if err != nil {
		t.Fatalf("reset mode should exit 0, got %v (output: %s)", err, out)
	}
	// The password must never appear in the process output.
	if strings.Contains(string(out), "newpassword1") {
		t.Fatalf("the new password leaked to output: %s", out)
	}

	// The reset took effect: new verifies, old does not.
	base := filepath.Join(dir, "data")
	key, _ := userstore.ResolveTOTPKey("", filepath.Join(base, "userstore.key"))
	s, err := userstore.Open(filepath.Join(base, "users.db"), key)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s.Close()
	u, err := s.GetUser("admin@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if ok, _ := userstore.VerifyPassword("newpassword1", u.PasswordHash); !ok {
		t.Fatal("the new password must verify after the startup-mode reset")
	}
	if ok, _ := userstore.VerifyPassword("oldpassword1", u.PasswordHash); ok {
		t.Fatal("the old password must NOT verify after the startup-mode reset")
	}
}

// TestStartupReset_NoPasswordFlag: there is NO `--password` flag — the new password is
// TTY/stdin-only, never argv. Passing one is rejected by the flag parser.
func TestStartupReset_NoPasswordFlag(t *testing.T) {
	bin := shokaBinary(t)
	dir := t.TempDir()
	cfgPath := writeResetConfig(t, dir)
	seedUser(t, dir, "admin@example.com", "oldpassword1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath,
		"--reset-password", "admin@example.com", "--password", "secret-on-argv")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a --password argv flag must be rejected (it does not exist); output: %s", out)
	}
	if !strings.Contains(string(out), "flag provided but not defined: -password") {
		t.Fatalf("expected an undefined-flag rejection for -password, got: %s", out)
	}
}
