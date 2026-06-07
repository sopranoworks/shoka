package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// loadWorkspace reads and parses the workspace JSON written by `workspace set`.
func loadWorkspace(t *testing.T, path string) workspaceConfig {
	t.Helper()
	var cfg workspaceConfig
	if err := json.Unmarshal([]byte(readFile(t, path)), &cfg); err != nil {
		t.Fatalf("parse workspace JSON %s: %v", path, err)
	}
	return cfg
}

// TestWorkspaceSetWritesJSON: a first write lands the assignment in
// .claude/shoka-workspace.json (relative to the working dir) with the exact shape
// the onboarding skill reads, and omits an empty environment.
func TestWorkspaceSetWritesJSON(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	if err := cmdWorkspaceSet([]string{"--namespace", "acme", "--project", "docs"}); err != nil {
		t.Fatalf("workspace set: %v", err)
	}
	path := filepath.Join(work, ".claude", "shoka-workspace.json")
	cfg := loadWorkspace(t, path)
	if cfg.Namespace != "acme" || cfg.Project != "docs" {
		t.Fatalf("assignment = %+v, want acme/docs", cfg)
	}
	// An empty environment must be omitted (omitempty), not written as "".
	if got := readFile(t, path); contains(got, "environment") {
		t.Fatalf("empty environment should be omitted; got %q", got)
	}
}

// TestWorkspaceSetEnvironmentAndGemini: --environment is recorded and --runtime
// gemini writes under .gemini/.
func TestWorkspaceSetEnvironmentAndGemini(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	if err := cmdWorkspaceSet([]string{"--namespace", "acme", "--project", "docs", "--environment", "prod", "--runtime", "gemini"}); err != nil {
		t.Fatalf("workspace set: %v", err)
	}
	path := filepath.Join(work, ".gemini", "shoka-workspace.json")
	cfg := loadWorkspace(t, path)
	if cfg.Namespace != "acme" || cfg.Project != "docs" || cfg.Environment != "prod" {
		t.Fatalf("assignment = %+v, want acme/docs env=prod", cfg)
	}
	// The claude convention dir must NOT have been written.
	if _, err := os.Stat(filepath.Join(work, ".claude", "shoka-workspace.json")); !os.IsNotExist(err) {
		t.Fatal("gemini runtime must not write the .claude workspace JSON")
	}
}

// TestWorkspaceSetRefusesOverwriteWithoutForce: existing-JSON behaviour — an
// existing assignment is protected; only --force overwrites it.
func TestWorkspaceSetRefusesOverwriteWithoutForce(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	if err := cmdWorkspaceSet([]string{"--namespace", "acme", "--project", "docs"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Second write without --force must error and must NOT change the file.
	if err := cmdWorkspaceSet([]string{"--namespace", "other", "--project", "thing"}); err == nil {
		t.Fatal("overwrite without --force must error")
	}
	path := filepath.Join(work, ".claude", "shoka-workspace.json")
	if cfg := loadWorkspace(t, path); cfg.Namespace != "acme" || cfg.Project != "docs" {
		t.Fatalf("file changed despite refused overwrite: %+v", cfg)
	}
	// With --force the overwrite succeeds.
	if err := cmdWorkspaceSet([]string{"--namespace", "other", "--project", "thing", "--force"}); err != nil {
		t.Fatalf("forced overwrite: %v", err)
	}
	if cfg := loadWorkspace(t, path); cfg.Namespace != "other" || cfg.Project != "thing" {
		t.Fatalf("forced overwrite did not apply: %+v", cfg)
	}
}

// TestWorkspaceSetRequiresNsProj: both --namespace and --project are required.
func TestWorkspaceSetRequiresNsProj(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := cmdWorkspaceSet([]string{"--namespace", "acme"}); err == nil {
		t.Fatal("missing --project must error")
	}
	if err := cmdWorkspaceSet([]string{"--project", "docs"}); err == nil {
		t.Fatal("missing --namespace must error")
	}
}

// TestWorkspaceSetUnknownRuntime: an unknown runtime is rejected (shared
// convention-dir resolver).
func TestWorkspaceSetUnknownRuntime(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := cmdWorkspaceSet([]string{"--namespace", "a", "--project", "b", "--runtime", "emacs"}); err == nil {
		t.Fatal("unknown runtime must error")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
