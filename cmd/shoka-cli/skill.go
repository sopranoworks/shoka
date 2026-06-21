package main

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/sopranoworks/shoka/internal/skillcache"
)

// cmdSkill dispatches the `skill` subcommand group — the apt-model skill
// distribution (B-15c):
//
//	skill update    = apt update  : git-fetch the remote skills repo into the cache
//	skill install   = apt install : copy a cached skill into a runtime convention dir
//	skill upgrade   = apt upgrade : re-copy installed skills whose cached version differs
//	skill outdated                : show installed skills that differ from the cache
//
// It is a thin client: git + filesystem only. It carries NO Shoka data-path
// access (never write_file/read_file/the allowlist) and NO connection config —
// skills are not data and are not tied to a Shoka endpoint. It also never writes
// the workspace JSON: install places skill FILES; establishing an agent's
// namespace/project assignment is a separate concern (B-15 steps c/d).
func cmdSkill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("skill: a subcommand is required (update, install, list, upgrade, outdated)")
	}
	switch args[0] {
	case "update":
		return cmdSkillUpdate(args[1:])
	case "install":
		return cmdSkillInstall(args[1:])
	case "list":
		return cmdSkillList(args[1:])
	case "upgrade":
		return cmdSkillUpgrade(args[1:])
	case "outdated":
		return cmdSkillOutdated(args[1:])
	default:
		return fmt.Errorf("unknown skill subcommand %q (expected: update, install, list, upgrade, outdated)", args[0])
	}
}

// cmdSkillUpdate is `apt update`: the one network op. It narrowly git-fetches the
// skills/ subtree of the skills repo into the local cache. The source defaults to
// the project's own public repo (skillcache.DefaultSkillsRepo); --repo overrides
// it (e.g. a local throwaway repo for testing, or an alternate skills source).
func cmdSkillUpdate(args []string) error {
	fs := flag.NewFlagSet("skill update", flag.ContinueOnError)
	repo := fs.String("repo", "", "skills repository (URL or local path) to sync from (default: the project repo "+skillcache.DefaultSkillsRepo+")")
	ref := fs.String("ref", "", "branch or tag to sync (default: the remote's default branch)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	source := skillcache.ResolveRepo(*repo)

	cacheDir, err := skillcache.DefaultCacheDir()
	if err != nil {
		return err
	}
	fmt.Printf("syncing skills from %s\n", source)
	res, err := skillcache.Sync(cacheDir, source, *ref)
	if err != nil {
		return err
	}

	fmt.Printf("synced skills cache at %s\n", cacheDir)
	reportSync("added", res.Added)
	reportSync("updated", res.Updated)
	reportSync("removed", res.Removed)
	if len(res.Added) == 0 && len(res.Updated) == 0 && len(res.Removed) == 0 {
		fmt.Printf("  up to date (%d skill(s) cached)\n", len(res.Unchanged))
	}
	return nil
}

func reportSync(label string, names []string) {
	for _, n := range names {
		fmt.Printf("  %s: %s\n", label, n)
	}
}

// cmdSkillInstall installs the Shoka skill SET. The Shoka skills are required
// tooling for an agent using Shoka, not an a-la-carte catalogue — so with NO name
// it installs EVERY skill currently in the synced cache (a fresh user need not
// know skill names). One or more explicit names install just those (the targeted
// case). It works OFFLINE (reads the already-synced cache); an empty cache or an
// un-cached name points at `skill update`. It places FILES only — never the
// workspace JSON. The set is data-driven: a skill added to the source repo's
// skills/ is installed automatically, with no CLI change.
func cmdSkillInstall(args []string) error {
	fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "install at the user level instead of the current working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cacheDir, err := skillcache.DefaultCacheDir()
	if err != nil {
		return err
	}
	destParent, err := skillsConventionDir(*runtime, *global)
	if err != nil {
		return err
	}

	names := fs.Args()
	if len(names) == 0 {
		// Whole-set install: every skill currently in the cache.
		all, lerr := skillcache.List(cacheDir)
		if lerr != nil {
			return lerr
		}
		if len(all) == 0 {
			return fmt.Errorf("no skills in the cache (%s); run `shoka-cli skill update` first", cacheDir)
		}
		names = all
	}

	for _, name := range names {
		if !skillcache.Has(cacheDir, name) {
			return fmt.Errorf("skill %q is not in the cache (%s); run `shoka-cli skill update` first", name, cacheDir)
		}
		dst, cerr := skillcache.CopySkill(filepath.Join(cacheDir, name), destParent)
		if cerr != nil {
			return cerr
		}
		fmt.Printf("installed %s -> %s\n", name, dst)
	}
	return nil
}

// cmdSkillList shows the skills available in the synced cache and whether each is
// installed in the target runtime's convention dir. It changes nothing — it makes
// the (data-driven) set visible; the primary path is still `skill install` with
// no name, which installs the whole set.
func cmdSkillList(args []string) error {
	fs := flag.NewFlagSet("skill list", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "inspect the user-level install location instead of the current working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cacheDir, err := skillcache.DefaultCacheDir()
	if err != nil {
		return err
	}
	cached, err := skillcache.List(cacheDir)
	if err != nil {
		return err
	}
	if len(cached) == 0 {
		fmt.Printf("no skills in the cache (%s); run `shoka-cli skill update` first\n", cacheDir)
		return nil
	}
	destParent, err := skillsConventionDir(*runtime, *global)
	if err != nil {
		return err
	}
	fmt.Printf("skills in the cache (%s):\n", cacheDir)
	for _, name := range cached {
		status := "not installed"
		if skillcache.Has(destParent, name) {
			status = "installed"
		}
		fmt.Printf("  %-32s %s\n", name, status)
	}
	fmt.Println("install the whole set with `shoka-cli skill install` (no name)")
	return nil
}

// cmdSkillOutdated shows installed skills whose installed content differs from
// the cache (the drift front). It changes nothing.
func cmdSkillOutdated(args []string) error {
	fs := flag.NewFlagSet("skill outdated", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "inspect the user-level install location instead of the current working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	drift, err := computeDrift(*runtime, *global)
	if err != nil {
		return err
	}
	if len(drift) == 0 {
		fmt.Println("all installed skills are up to date with the cache")
		return nil
	}
	fmt.Println("outdated skills (installed content differs from the cache):")
	for _, d := range drift {
		fmt.Printf("  %s\n", d)
	}
	fmt.Println("run `shoka-cli skill upgrade` to update them")
	return nil
}

// cmdSkillUpgrade is `apt upgrade`: re-copy from the cache every installed skill
// whose cached content differs. Offline (reads the cache).
func cmdSkillUpgrade(args []string) error {
	fs := flag.NewFlagSet("skill upgrade", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "upgrade the user-level install location instead of the current working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cacheDir, err := skillcache.DefaultCacheDir()
	if err != nil {
		return err
	}
	destParent, err := skillsConventionDir(*runtime, *global)
	if err != nil {
		return err
	}
	drift, err := computeDrift(*runtime, *global)
	if err != nil {
		return err
	}
	if len(drift) == 0 {
		fmt.Println("nothing to upgrade; all installed skills match the cache")
		return nil
	}
	for _, name := range drift {
		dst, cerr := skillcache.CopySkill(filepath.Join(cacheDir, name), destParent)
		if cerr != nil {
			return fmt.Errorf("upgrade %s: %w", name, cerr)
		}
		fmt.Printf("upgraded %s -> %s\n", name, dst)
	}
	return nil
}

// computeDrift returns the names of skills that are installed in the runtime's
// convention dir, also present in the cache, and whose installed content hash
// differs from the cached one. Skills installed but absent from the cache are
// skipped (nothing to compare against — the cache may simply not be synced).
func computeDrift(runtime string, global bool) ([]string, error) {
	cacheDir, err := skillcache.DefaultCacheDir()
	if err != nil {
		return nil, err
	}
	destParent, err := skillsConventionDir(runtime, global)
	if err != nil {
		return nil, err
	}
	installed, err := skillcache.List(destParent)
	if err != nil {
		return nil, err
	}
	var drift []string
	for _, name := range installed {
		if !skillcache.Has(cacheDir, name) {
			continue
		}
		instHash, herr := skillcache.DirHash(filepath.Join(destParent, name))
		if herr != nil {
			return nil, herr
		}
		cacheHash, herr := skillcache.DirHash(filepath.Join(cacheDir, name))
		if herr != nil {
			return nil, herr
		}
		if instHash != cacheHash {
			drift = append(drift, name)
		}
	}
	return drift, nil
}

// skillsConventionDir resolves the skills directory for a runtime. The skill is
// later placed at <returned>/<name>/. --global selects the user-level location;
// otherwise it is relative to the current working directory. It builds on the
// shared conventionDir resolver so the runtime→path mapping is defined once:
//
//	claude -> .claude/skills   (~/.claude/skills with --global)
//	gemini -> .gemini/skills   (~/.gemini/skills with --global)
func skillsConventionDir(runtime string, global bool) (string, error) {
	base, err := conventionDir(runtime, global)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "skills"), nil
}
