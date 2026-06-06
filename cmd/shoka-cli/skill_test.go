package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSkillAptCycle proves the apt-model skill distribution end to end against a
// LOCAL THROWAWAY git repository as the remote (no real remote, no deployment
// detail): the actual cmdSkill subcommands clone the remote into the cache, copy
// a skill into a runtime convention dir, detect drift after the remote changes,
// and upgrade. The fetch is the git binary (os/exec) — there is no go-git here,
// keeping cmd/shoka-cli archlint-clean (Anchor 2).
func TestSkillAptCycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available; skill distribution shells out to git")
	}

	// Isolate the cache: os.UserCacheDir resolves under $HOME/Library/Caches on
	// macOS and $XDG_CACHE_HOME on Linux — set both so DefaultCacheDir lands in a
	// temp tree on either platform.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	// A local throwaway "remote" skills repo with one skill + a non-skill README.
	remote := t.TempDir()
	writeFile(t, filepath.Join(remote, "demo-skill", "SKILL.md"), "# Demo Skill\nv1\n")
	writeFile(t, filepath.Join(remote, "demo-skill", "helper.txt"), "helper one\n")
	writeFile(t, filepath.Join(remote, "README.md"), "# repo readme, not a skill\n")
	gitInitCommit(t, remote, "v1")

	// (1) skill update — the one network op: clone the remote into the cache.
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill update: %v", err)
	}

	// Install/upgrade resolve the convention dir relative to the working dir.
	proj := t.TempDir()
	t.Chdir(proj)

	// (2) skill install — copy the cached skill into .claude/skills/<name>/.
	if err := cmdSkill([]string{"install", "demo-skill"}); err != nil {
		t.Fatalf("skill install: %v", err)
	}
	installed := filepath.Join(proj, ".claude", "skills", "demo-skill")
	if got := readFile(t, filepath.Join(installed, "SKILL.md")); got != "# Demo Skill\nv1\n" {
		t.Fatalf("installed SKILL.md mismatch: got %q", got)
	}
	if readFile(t, filepath.Join(installed, "helper.txt")) != "helper one\n" {
		t.Fatal("installed helper.txt missing/mismatch")
	}
	// install must NOT write the workspace JSON (assignment is a separate concern).
	if _, err := os.Stat(filepath.Join(proj, ".claude", "shoka-workspace.json")); !os.IsNotExist(err) {
		t.Fatal("skill install must not write the workspace JSON")
	}

	// (3) freshly installed => no drift.
	if drift, err := computeDrift("claude", false); err != nil || len(drift) != 0 {
		t.Fatalf("expected no drift after install; got %v err=%v", drift, err)
	}

	// (4) Change the remote: edit a file AND add a new supporting file (drift must
	// catch added files, not just SKILL.md edits), then re-update.
	writeFile(t, filepath.Join(remote, "demo-skill", "SKILL.md"), "# Demo Skill\nv2 CHANGED\n")
	writeFile(t, filepath.Join(remote, "demo-skill", "extra.txt"), "added in v2\n")
	gitInitCommit(t, remote, "v2")
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill re-update: %v", err)
	}

	// (5) Now demo-skill is outdated.
	drift, err := computeDrift("claude", false)
	if err != nil {
		t.Fatalf("computeDrift: %v", err)
	}
	if len(drift) != 1 || drift[0] != "demo-skill" {
		t.Fatalf("expected demo-skill outdated; got %v", drift)
	}

	// (6) Upgrade: re-copy and clean-replace (the v2 content, plus the new file).
	if err := cmdSkill([]string{"upgrade"}); err != nil {
		t.Fatalf("skill upgrade: %v", err)
	}
	if got := readFile(t, filepath.Join(installed, "SKILL.md")); got != "# Demo Skill\nv2 CHANGED\n" {
		t.Fatalf("post-upgrade SKILL.md mismatch: got %q", got)
	}
	if readFile(t, filepath.Join(installed, "extra.txt")) != "added in v2\n" {
		t.Fatal("post-upgrade extra.txt not copied")
	}
	if drift, err := computeDrift("claude", false); err != nil || len(drift) != 0 {
		t.Fatalf("expected no drift after upgrade; got %v err=%v", drift, err)
	}

	// (7) gemini runtime install lands under .gemini/skills/<name>/.
	if err := cmdSkill([]string{"install", "--runtime", "gemini", "demo-skill"}); err != nil {
		t.Fatalf("gemini install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".gemini", "skills", "demo-skill", "SKILL.md")); err != nil {
		t.Fatalf("gemini install did not place the skill: %v", err)
	}
}

// TestSkillInstallUncachedErrors proves the apt separation: install of a skill
// not in the cache errors and points at `skill update` (no implicit fetch).
func TestSkillInstallUncachedErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Chdir(t.TempDir())

	// No cache synced at all: install must error (and not panic).
	if err := cmdSkill([]string{"install", "ghost"}); err == nil {
		t.Fatal("install of an un-cached skill must error")
	}
}

// TestSkillUpdateRequiresRepo proves --repo is required (no baked-in default).
func TestSkillUpdateRequiresRepo(t *testing.T) {
	if err := cmdSkill([]string{"update"}); err == nil {
		t.Fatal("skill update without --repo must error")
	}
}

// TestSkillUnknownRuntime rejects an unknown --runtime.
func TestSkillUnknownRuntime(t *testing.T) {
	if _, err := skillsConventionDir("emacs", false); err == nil {
		t.Fatal("unknown runtime must error")
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gitInitCommit inits the repo if needed and commits all changes with msg, using
// the git binary (os/exec) — never go-git. Identity is set per-command so no
// global git config is required in the test environment.
func gitInitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		runTestGit(t, dir, "-c", "init.defaultBranch=main", "init", "-q")
	}
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-q", "-m", msg)
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
