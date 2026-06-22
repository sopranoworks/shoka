package main

import (
	"flag"
	"fmt"
)

// cmdSkill dispatches the `skill` subcommand group — the apt-model skill
// distribution. The actual machinery lives in the reusable github.com/sopranoworks/
// skilldist library; this file is the THIN Shoka CLI layer (dispatch, flag parsing,
// user messages) that wraps it with Shoka's injected Config (see skilldist.go).
//
//	skill update       = apt update  : git-fetch the skills/ subtree into the cache
//	skill install      = apt install : copy the cached skill SET into a runtime convention dir
//	skill list                       : show the cached set + install status
//	skill outdated                   : show installed skills that differ from the cache
//	skill upgrade      = apt upgrade : re-copy installed skills whose cached version differs
//	skill make-current               : one step — refresh + upgrade + prune (hides the cache)
//	skill prune                      : remove installed skills deleted upstream (only ones we own)
//
// It is a thin client: git + filesystem only, via the library. It carries NO Shoka
// data-path access and NO connection config — skills are not data. It never writes
// the workspace JSON: install places skill FILES; the namespace/project assignment
// is a separate concern (B-15 steps c/d).
func cmdSkill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("skill: a subcommand is required (update, install, list, outdated, upgrade, make-current, prune)")
	}
	switch args[0] {
	case "update":
		return cmdSkillUpdate(args[1:])
	case "install":
		return cmdSkillInstall(args[1:])
	case "list":
		return cmdSkillList(args[1:])
	case "outdated":
		return cmdSkillOutdated(args[1:])
	case "upgrade":
		return cmdSkillUpgrade(args[1:])
	case "make-current":
		return cmdSkillMakeCurrent(args[1:])
	case "prune":
		return cmdSkillPrune(args[1:])
	default:
		return fmt.Errorf("unknown skill subcommand %q (expected: update, install, list, outdated, upgrade, make-current, prune)", args[0])
	}
}

// cmdSkillUpdate is `apt update`: the one network op. It narrowly git-fetches the
// skills/ subtree of the source repo into the local cache. The source defaults to
// the project's own public repo (DefaultSkillsRepo); --repo overrides it.
func cmdSkillUpdate(args []string) error {
	fs := flag.NewFlagSet("skill update", flag.ContinueOnError)
	repo := fs.String("repo", "", "skills repository (URL or local path) to sync from (default: the project repo "+DefaultSkillsRepo+")")
	ref := fs.String("ref", "", "branch or tag to sync (default: the remote's default branch)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// update needs no convention dir; build a source-only config.
	cfg, err := skilldistConfig("claude", false, *repo)
	if err != nil {
		return err
	}
	cacheDir, err := cfg.CacheDir()
	if err != nil {
		return err
	}
	fmt.Printf("syncing skills from %s\n", cfg.Source)
	res, err := cfg.Sync(*ref)
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

// cmdSkillInstall installs the Shoka skill SET. With NO name it installs EVERY
// skill currently in the synced cache (required tooling, not an a-la-carte
// catalogue); one or more explicit names install just those. Offline; an empty
// cache or an un-cached name points at `skill update`. Files only — never the
// workspace JSON. Data-driven: a skill added to the source is installed
// automatically.
func cmdSkillInstall(args []string) error {
	fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "install at the user level instead of the current working directory")
	repo := fs.String("repo", "", "which source's cached set to install (default: the project repo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	names := fs.Args()
	var installed []string
	if len(names) == 0 {
		installed, err = cfg.InstallSet()
	} else {
		installed, err = cfg.Install(names...)
	}
	if err != nil {
		return err
	}
	for _, n := range installed {
		fmt.Printf("installed %s -> %s/%s\n", n, cfg.ConventionPath, n)
	}
	return nil
}

// cmdSkillList shows the cached set and whether each skill is installed.
func cmdSkillList(args []string) error {
	fs := flag.NewFlagSet("skill list", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "inspect the user-level install location instead of the current working directory")
	repo := fs.String("repo", "", "which source's cached set to list (default: the project repo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	cacheDir, err := cfg.CacheDir()
	if err != nil {
		return err
	}
	cached, err := cfg.ListCache()
	if err != nil {
		return err
	}
	if len(cached) == 0 {
		fmt.Printf("no skills in the cache (%s); run `shoka-cli skill update` first\n", cacheDir)
		return nil
	}
	installed, err := cfg.ListInstalled()
	if err != nil {
		return err
	}
	installedSet := make(map[string]bool, len(installed))
	for _, n := range installed {
		installedSet[n] = true
	}
	fmt.Printf("skills in the cache (%s):\n", cacheDir)
	for _, name := range cached {
		status := "not installed"
		if installedSet[name] {
			status = "installed"
		}
		fmt.Printf("  %-32s %s\n", name, status)
	}
	fmt.Println("install the whole set with `shoka-cli skill install` (no name)")
	return nil
}

// cmdSkillOutdated shows installed skills whose installed content differs from the
// cache (the drift front). It changes nothing.
func cmdSkillOutdated(args []string) error {
	fs := flag.NewFlagSet("skill outdated", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "inspect the user-level install location instead of the current working directory")
	repo := fs.String("repo", "", "which source's cache to compare against (default: the project repo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	drift, err := cfg.Outdated()
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
// whose cached content differs. Offline.
func cmdSkillUpgrade(args []string) error {
	fs := flag.NewFlagSet("skill upgrade", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "upgrade the user-level install location instead of the current working directory")
	repo := fs.String("repo", "", "which source's cache to upgrade from (default: the project repo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	upgraded, err := cfg.Upgrade()
	if err != nil {
		return err
	}
	if len(upgraded) == 0 {
		fmt.Println("nothing to upgrade; all installed skills match the cache")
		return nil
	}
	for _, n := range upgraded {
		fmt.Printf("upgraded %s -> %s/%s\n", n, cfg.ConventionPath, n)
	}
	return nil
}

// cmdSkillMakeCurrent is the one-step "be current": refresh from the source
// (ls-remote gated, --force bypasses), upgrade drifted installed skills, and prune
// skills removed upstream — without the operator running update+upgrade+prune or
// thinking about the cache.
func cmdSkillMakeCurrent(args []string) error {
	fs := flag.NewFlagSet("skill make-current", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "operate on the user-level install location instead of the current working directory")
	repo := fs.String("repo", "", "skills repository to refresh from (default: the project repo)")
	ref := fs.String("ref", "", "branch or tag to refresh (default: the remote's default branch)")
	force := fs.Bool("force", false, "refresh even if the cached commit already matches the remote")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	fmt.Printf("making skills current from %s\n", cfg.Source)
	rep, err := cfg.MakeCurrent(*ref, *force)
	if err != nil {
		return err
	}
	if rep.Synced {
		reportSync("added", rep.Sync.Added)
		reportSync("updated", rep.Sync.Updated)
		reportSync("removed", rep.Sync.Removed)
	} else {
		fmt.Println("  cache already current with the source")
	}
	for _, n := range rep.Upgraded {
		fmt.Printf("  upgraded: %s\n", n)
	}
	for _, n := range rep.Pruned {
		fmt.Printf("  pruned: %s\n", n)
	}
	if len(rep.Upgraded) == 0 && len(rep.Pruned) == 0 {
		fmt.Println("  installed skills already current")
	}
	return nil
}

// cmdSkillPrune removes installed skills that are no longer in the synced set AND
// are provably Shoka-distributed (our signature). It never touches a skill we do
// not own. Run `skill update` first so the cache reflects the current source.
func cmdSkillPrune(args []string) error {
	fs := flag.NewFlagSet("skill prune", flag.ContinueOnError)
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "operate on the user-level install location instead of the current working directory")
	repo := fs.String("repo", "", "which source's set defines what is current (default: the project repo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skilldistConfig(*runtime, *global, *repo)
	if err != nil {
		return err
	}
	pruned, err := cfg.Prune()
	if err != nil {
		return err
	}
	if len(pruned) == 0 {
		fmt.Println("nothing to prune; no managed skills were removed upstream")
		return nil
	}
	for _, n := range pruned {
		fmt.Printf("pruned %s\n", n)
	}
	return nil
}
