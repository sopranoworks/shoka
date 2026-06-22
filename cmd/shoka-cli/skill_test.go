package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sopranoworks/skilldist/skillmeta"
)

// These tests drive the actual cmdSkill subcommands against a LOCAL THROWAWAY git
// repository as the source (no real remote, no network egress). The machinery now
// lives in the reusable github.com/sopranoworks/skilldist library; this file
// exercises the Shoka CLI layer + its injected Config (skilldist.go). The fetch is
// the git binary (os/exec) — no go-git, keeping cmd/shoka-cli archlint-clean.

// TestSkillAptCycle: update (narrow, source-namespaced cache) -> install -> no
// drift -> change source -> re-update -> outdated -> upgrade -> no drift, plus the
// gemini runtime. Behaviour-preserving for the existing verbs.
func TestSkillAptCycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available; skill distribution shells out to git")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	// A local throwaway "remote" laid out like the FIXED repo: skills under skills/,
	// plus decoys that the narrow fetch must NOT bring into the cache.
	remote := t.TempDir()
	writeFile(t, filepath.Join(remote, "skills", "demo-skill", "SKILL.md"), "# Demo Skill\nv1\n")
	writeFile(t, filepath.Join(remote, "skills", "demo-skill", "helper.txt"), "helper one\n")
	writeFile(t, filepath.Join(remote, "README.md"), "# repo readme, not a skill\n")
	writeFile(t, filepath.Join(remote, "cmd", "shoka", "main.go"), "package main\n")
	gitInitCommit(t, remote, "v1")

	// (1) update — narrow fetch into the source-namespaced cache.
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill update: %v", err)
	}
	cfg, err := skilldistConfig("claude", false, remote)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir, err := cfg.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "demo-skill", "SKILL.md")); err != nil {
		t.Fatalf("cache missing demo-skill: %v", err)
	}
	for _, decoy := range []string{".git", "cmd", "README.md", "skills"} {
		if _, err := os.Stat(filepath.Join(cacheDir, decoy)); !os.IsNotExist(err) {
			t.Fatalf("cache must not contain %q (narrow fetch); stat err=%v", decoy, err)
		}
	}

	proj := t.TempDir()
	t.Chdir(proj)

	// (2) install the named skill.
	if err := cmdSkill([]string{"install", "--repo", remote, "demo-skill"}); err != nil {
		t.Fatalf("skill install: %v", err)
	}
	installed := filepath.Join(proj, ".claude", "skills", "demo-skill")
	if got := readFile(t, filepath.Join(installed, "SKILL.md")); got != "# Demo Skill\nv1\n" {
		t.Fatalf("installed SKILL.md mismatch: got %q", got)
	}
	if readFile(t, filepath.Join(installed, "helper.txt")) != "helper one\n" {
		t.Fatal("installed helper.txt missing/mismatch")
	}
	// install must NOT write the workspace JSON.
	if _, err := os.Stat(filepath.Join(proj, ".claude", "shoka-workspace.json")); !os.IsNotExist(err) {
		t.Fatal("skill install must not write the workspace JSON")
	}

	// (3) freshly installed => no drift.
	projCfg, err := skilldistConfig("claude", false, remote)
	if err != nil {
		t.Fatal(err)
	}
	if drift, err := projCfg.Outdated(); err != nil || len(drift) != 0 {
		t.Fatalf("expected no drift after install; got %v err=%v", drift, err)
	}

	// (4) change the source: edit + add a file, re-update.
	writeFile(t, filepath.Join(remote, "skills", "demo-skill", "SKILL.md"), "# Demo Skill\nv2 CHANGED\n")
	writeFile(t, filepath.Join(remote, "skills", "demo-skill", "extra.txt"), "added in v2\n")
	gitInitCommit(t, remote, "v2")
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill re-update: %v", err)
	}

	// (5) now demo-skill is outdated.
	drift, err := projCfg.Outdated()
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if len(drift) != 1 || drift[0] != "demo-skill" {
		t.Fatalf("expected demo-skill outdated; got %v", drift)
	}

	// (6) upgrade: re-copy (v2 content + the new file).
	if err := cmdSkill([]string{"upgrade", "--repo", remote}); err != nil {
		t.Fatalf("skill upgrade: %v", err)
	}
	if got := readFile(t, filepath.Join(installed, "SKILL.md")); got != "# Demo Skill\nv2 CHANGED\n" {
		t.Fatalf("post-upgrade SKILL.md mismatch: got %q", got)
	}
	if readFile(t, filepath.Join(installed, "extra.txt")) != "added in v2\n" {
		t.Fatal("post-upgrade extra.txt not copied")
	}
	if drift, err := projCfg.Outdated(); err != nil || len(drift) != 0 {
		t.Fatalf("expected no drift after upgrade; got %v err=%v", drift, err)
	}

	// (7) gemini runtime install lands under .gemini/skills/<name>/.
	if err := cmdSkill([]string{"install", "--runtime", "gemini", "--repo", remote, "demo-skill"}); err != nil {
		t.Fatalf("gemini install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".gemini", "skills", "demo-skill", "SKILL.md")); err != nil {
		t.Fatalf("gemini install did not place the skill: %v", err)
	}
}

// TestSkillInstallUncachedErrors: install of a skill not in the cache errors and
// points at `skill update` (no implicit fetch).
func TestSkillInstallUncachedErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Chdir(t.TempDir())

	if err := cmdSkill([]string{"install", "ghost"}); err == nil {
		t.Fatal("install of an un-cached skill must error")
	}
}

// TestResolveRepoDefault: --repo is OPTIONAL; no flag resolves to the baked fixed
// project repo; an override wins. Asserts the resolver only — no fetch.
func TestResolveRepoDefault(t *testing.T) {
	if got := resolveRepo(""); got != DefaultSkillsRepo {
		t.Fatalf("no --repo must resolve to the default %q; got %q", DefaultSkillsRepo, got)
	}
	if got := resolveRepo("/tmp/throwaway"); got != "/tmp/throwaway" {
		t.Fatalf("--repo override must win; got %q", got)
	}
}

// TestSkillInstallWholeSet: `install` with NO name installs EVERY cached skill, and
// the set is DATA-DRIVEN — a skill added to the source is picked up with no CLI
// change.
func TestSkillInstallWholeSet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	remote := t.TempDir()
	writeFile(t, filepath.Join(remote, "skills", "alpha", "SKILL.md"), "# alpha\n")
	writeFile(t, filepath.Join(remote, "skills", "beta", "SKILL.md"), "# beta\n")
	gitInitCommit(t, remote, "two skills")
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill update: %v", err)
	}

	work := t.TempDir()
	t.Chdir(work)

	if err := cmdSkill([]string{"install", "--repo", remote}); err != nil {
		t.Fatalf("skill install (whole set): %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(work, ".claude", "skills", name, "SKILL.md")); err != nil {
			t.Fatalf("whole-set install missing %s: %v", name, err)
		}
	}
	if err := cmdSkill([]string{"list", "--repo", remote}); err != nil {
		t.Fatalf("skill list: %v", err)
	}

	// Data-driven: add a THIRD skill upstream; update + install (no name) picks it up.
	writeFile(t, filepath.Join(remote, "skills", "gamma", "SKILL.md"), "# gamma\n")
	gitInitCommit(t, remote, "add gamma")
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatalf("skill re-update: %v", err)
	}
	if err := cmdSkill([]string{"install", "--repo", remote}); err != nil {
		t.Fatalf("skill install after add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "skills", "gamma", "SKILL.md")); err != nil {
		t.Fatalf("data-driven set did not pick up gamma: %v", err)
	}
}

// TestSkillPruneAndMakeCurrent: a skill removed upstream is pruned ONLY when it
// carries Shoka's signature; make-current refreshes+upgrades+prunes in one step.
func TestSkillPruneAndMakeCurrent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	remote := t.TempDir()
	addSignedSkill(t, remote, "alpha", "# alpha\n")
	addSignedSkill(t, remote, "beta", "# beta\n")
	gitInitCommit(t, remote, "alpha+beta")

	work := t.TempDir()
	t.Chdir(work)
	if err := cmdSkill([]string{"update", "--repo", remote}); err != nil {
		t.Fatal(err)
	}
	if err := cmdSkill([]string{"install", "--repo", remote}); err != nil {
		t.Fatal(err)
	}

	// A foreign skill the prune must NEVER touch (no Shoka signature).
	writeFile(t, filepath.Join(work, ".claude", "skills", "foreign", "SKILL.md"), "# foreign\n")

	// Remove beta upstream; make-current should refresh, then prune beta only.
	if err := os.RemoveAll(filepath.Join(remote, "skills", "beta")); err != nil {
		t.Fatal(err)
	}
	gitInitCommit(t, remote, "remove beta")
	if err := cmdSkill([]string{"make-current", "--repo", remote}); err != nil {
		t.Fatalf("make-current: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "skills", "beta")); !os.IsNotExist(err) {
		t.Fatal("beta should have been pruned by make-current")
	}
	for _, keep := range []string{"alpha", "foreign"} {
		if _, err := os.Stat(filepath.Join(work, ".claude", "skills", keep, "SKILL.md")); err != nil {
			t.Fatalf("make-current wrongly removed %s: %v", keep, err)
		}
	}
}

// TestSkillUnknownRuntime rejects an unknown --runtime.
func TestSkillUnknownRuntime(t *testing.T) {
	if _, err := skillsConventionDir("emacs", false); err == nil {
		t.Fatal("unknown runtime must error")
	}
}

// --- helpers (shared with init_test.go) ---

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

// addSignedSkill writes a skill under <repo>/skills/<name>/ and stamps its
// .skill-meta.yaml with Shoka's signature (so prune recognises it as ours).
func addSignedSkill(t *testing.T, repo, name, body string) {
	t.Helper()
	dir := filepath.Join(repo, "skills", name)
	writeFile(t, filepath.Join(dir, "SKILL.md"), body)
	m, err := skillmeta.Build(dir, skillSignature, skillmeta.Source{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := skillmeta.Write(dir, m); err != nil {
		t.Fatal(err)
	}
}

// gitInitCommit inits the repo if needed and commits all changes with msg, using
// the git binary (os/exec) — never go-git. Identity is set per-command so no global
// git config is required.
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
