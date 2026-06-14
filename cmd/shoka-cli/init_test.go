package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sopranoworks/shoka/internal/clientconfig"
)

// initTestEnv isolates the config dir, the cache dir, and the working dir so init's
// three phases write only into temp trees on either platform. It returns the working
// dir and a path to a token file holding a placeholder token (auth reads from a file,
// never argv). A local throwaway git repo carrying the wired skill pair is created and
// its path returned as the --repo for the skill phase.
func initTestEnv(t *testing.T) (work, tokenFile, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available; init's skill phase shells out to git")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	// A throwaway remote skills repo with the default wired pair.
	repo = t.TempDir()
	writeFile(t, filepath.Join(repo, "shoka-directive-onboarding", "SKILL.md"), "# Onboarding\n")
	writeFile(t, filepath.Join(repo, "shoka-workspace-setup", "SKILL.md"), "# Setup\n")
	gitInitCommit(t, repo, "skills v1")

	tokenFile = filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "placeholder-access-token\n")

	work = t.TempDir()
	t.Chdir(work)
	return work, tokenFile, repo
}

// TestInitRunsAllPhasesInOrder: a fresh init configures the connection, installs the
// wired skill pair, and writes the workspace assignment.
func TestInitRunsAllPhasesInOrder(t *testing.T) {
	work, tokenFile, repo := initTestEnv(t)

	err := cmdInit([]string{
		"--endpoint", "https://example.invalid/mcp",
		"--token-file", tokenFile,
		"--repo", repo,
		"--namespace", "acme", "--project", "docs",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Phase 1 — connection configured.
	cfg, err := clientconfig.Load(clientconfig.DefaultEnvironment)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if cfg.Endpoint != "https://example.invalid/mcp" || cfg.Token != "placeholder-access-token" {
		t.Fatalf("config = %+v, want endpoint+token set", cfg)
	}
	// Phase 2 — both wired skills installed.
	for _, name := range defaultInitSkills {
		if _, err := os.Stat(filepath.Join(work, ".claude", "skills", name, "SKILL.md")); err != nil {
			t.Fatalf("skill %s not installed: %v", name, err)
		}
	}
	// Phase 3 — assignment written.
	cw := loadWorkspace(t, filepath.Join(work, ".claude", "shoka-workspace.json"))
	if cw.Namespace != "acme" || cw.Project != "docs" {
		t.Fatalf("assignment = %+v, want acme/docs", cw)
	}
}

// TestInitIdempotentReRun: a second init with the connection already configured and
// the assignment already established skips both phases — crucially without a token
// file, proving the config phase does NOT block on stdin when already configured.
func TestInitIdempotentReRun(t *testing.T) {
	work, tokenFile, repo := initTestEnv(t)

	base := []string{
		"--endpoint", "https://example.invalid/mcp",
		"--token-file", tokenFile,
		"--repo", repo,
		"--namespace", "acme", "--project", "docs",
	}
	if err := cmdInit(base); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// Re-run with NO --token-file and a DIFFERENT ns/proj: config skips (already
	// configured, so it never reads stdin) and workspace skips (already exists), so
	// the different ns/proj is NOT applied.
	if err := cmdInit([]string{"--repo", repo, "--namespace", "changed", "--project", "changed"}); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	cw := loadWorkspace(t, filepath.Join(work, ".claude", "shoka-workspace.json"))
	if cw.Namespace != "acme" || cw.Project != "docs" {
		t.Fatalf("re-run changed the established assignment: %+v", cw)
	}
}

// TestInitReconfigureForcesWorkspace: --reconfigure overwrites the established
// assignment (and re-auths, so a token file is supplied).
func TestInitReconfigureForcesWorkspace(t *testing.T) {
	work, tokenFile, repo := initTestEnv(t)

	base := []string{
		"--endpoint", "https://example.invalid/mcp",
		"--token-file", tokenFile,
		"--repo", repo,
		"--namespace", "acme", "--project", "docs",
	}
	if err := cmdInit(base); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := cmdInit([]string{
		"--reconfigure",
		"--endpoint", "https://example.invalid/mcp",
		"--token-file", tokenFile,
		"--repo", repo,
		"--namespace", "newns", "--project", "newproj",
	}); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cw := loadWorkspace(t, filepath.Join(work, ".claude", "shoka-workspace.json"))
	if cw.Namespace != "newns" || cw.Project != "newproj" {
		t.Fatalf("--reconfigure did not overwrite the assignment: %+v", cw)
	}
}

// TestInitSkipFlags: --no-setup-config and --no-install-skill skip those phases;
// only the workspace assignment is written.
func TestInitSkipFlags(t *testing.T) {
	work, _, _ := initTestEnv(t)

	if err := cmdInit([]string{
		"--no-setup-config", "--no-install-skill",
		"--namespace", "acme", "--project", "docs",
	}); err != nil {
		t.Fatalf("init with skips: %v", err)
	}
	// Config phase skipped — no config written.
	if _, err := clientconfig.Load(clientconfig.DefaultEnvironment); err == nil {
		t.Fatal("config-setup was skipped but a config was written")
	}
	// Skill phase skipped — no skills installed.
	if _, err := os.Stat(filepath.Join(work, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("skill phase was skipped but skills were installed")
	}
	// Workspace phase still ran.
	cw := loadWorkspace(t, filepath.Join(work, ".claude", "shoka-workspace.json"))
	if cw.Namespace != "acme" || cw.Project != "docs" {
		t.Fatalf("assignment = %+v, want acme/docs", cw)
	}
}
