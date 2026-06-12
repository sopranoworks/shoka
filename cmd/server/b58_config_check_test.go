package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B-58 — the config dry-run (`shoka --config-check`) loads + validates the config and
// reports OK / the exact error WITHOUT starting the server or binding any port.
// runConfigCheck is the testable core: it calls config.Load (which binds nothing) and
// writes the result; main() maps its error to a non-zero exit. These tests cover the
// directive §5 dry-run OK + error paths.

func writeCheckConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shoka.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// OK path: a valid config → nil error, `config OK`, and an address-free summary.
func TestRunConfigCheck_ValidConfig_OKAndNoSecret(t *testing.T) {
	path := writeCheckConfig(t, `
server:
  http:
    listen: "127.0.0.1:8080"
  mcp:
    oauth:
      listen: "127.0.0.1:8082"
      external_url: "https://public.example"
      consent_credential: "TOP-SECRET-CONSENT"
  debug:
    dump_http: true
storage:
  base_dir: "/tmp/shoka-b58"
`)
	var out strings.Builder
	if err := runConfigCheck(path, &out); err != nil {
		t.Fatalf("valid config must pass --config-check: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "config OK") {
		t.Errorf("missing `config OK` line:\n%s", got)
	}
	if !strings.Contains(got, "http dump: enabled") {
		t.Errorf("summary should report the dump state:\n%s", got)
	}
	if !strings.Contains(got, "mcp-oauth") {
		t.Errorf("summary should name the opened transport surface:\n%s", got)
	}
	// Confidentiality: the summary names categories/booleans, never a secret or address.
	for _, leak := range []string{"TOP-SECRET-CONSENT", "127.0.0.1", "8082", "public.example"} {
		if strings.Contains(got, leak) {
			t.Errorf("check output leaked %q (must name categories only):\n%s", leak, got)
		}
	}
}

// Error path 1: an unknown key → non-nil error naming the key (main exits non-zero).
func TestRunConfigCheck_UnknownKey_Errors(t *testing.T) {
	path := writeCheckConfig(t, `
server:
  http:
    listen: ":8080"
  mcp:
    plain:
      listen: ":8081"
  dump_http: true     # misplaced — belongs under server.debug
storage:
  base_dir: "/tmp/shoka"
`)
	err := runConfigCheck(path, &strings.Builder{})
	if err == nil {
		t.Fatal("a misplaced key must make --config-check fail")
	}
	if !strings.Contains(err.Error(), "dump_http") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// Error path 2: a semantic failure (neither MCP transport — B-50 rule) → non-nil error.
func TestRunConfigCheck_SemanticFailure_NeitherMCPPort_Errors(t *testing.T) {
	path := writeCheckConfig(t, `
server:
  http:
    listen: ":8080"
storage:
  base_dir: "/tmp/shoka"
`)
	err := runConfigCheck(path, &strings.Builder{})
	if err == nil {
		t.Fatal("a config with no MCP transport must fail validation")
	}
	if !strings.Contains(err.Error(), "MCP transport") {
		t.Errorf("error should explain the neither-MCP-port failure, got: %v", err)
	}
}
