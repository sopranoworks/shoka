package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sopranoworks/shoka/internal/clientconfig"
)

// stringSliceFlag collects a repeatable string flag (e.g. --skill A --skill B).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return fmt.Sprintf("%v", []string(*s)) }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cmdInit is the one-shot ORCHESTRATOR (git init-like). It runs the three setup
// phases in order — config-setup (the connection), skill update+install (the skill
// layer), workspace set (the assignment) — by INVOKING the existing per-phase
// subcommands. It adds NO domain logic of its own: it assembles arg slices and calls
// cmdAuth / cmdSkillUpdate / cmdSkillInstall / cmdWorkspaceSet, which all remain
// independently callable. The skip checks below are orchestration (reads only), not
// re-implementations.
//
// Idempotency (operator-confirmed, git-init-like): an already-established phase is
// reported and skipped, not failed; --reconfigure forces re-running established
// phases. --no-setup-config / --no-install-skill skip a phase entirely. The apt
// skill lifecycle (update/upgrade) is NOT absorbed: init invokes install once; the
// lifecycle stays in the `skill` family.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	noConfig := fs.Bool("no-setup-config", false, "skip the config-setup (connection) phase")
	noSkill := fs.Bool("no-install-skill", false, "skip the skill update+install phase")
	reconfigure := fs.Bool("reconfigure", false, "force re-running already-established phases (re-auth, overwrite the workspace JSON)")

	// config-setup (auth) pass-throughs.
	env := fs.String("env", clientconfig.DefaultEnvironment, "connection environment name")
	endpoint := fs.String("endpoint", "", "MCP endpoint URL (for the config-setup phase)")
	tokenFile := fs.String("token-file", "", "read the auth token from this file instead of stdin")

	// skill pass-throughs.
	repo := fs.String("repo", "", "remote skills repository (URL or path) for the skill phase")
	ref := fs.String("ref", "", "branch or tag to sync for the skill phase")
	var skills stringSliceFlag
	fs.Var(&skills, "skill", "skill to install (repeatable; default: the whole synced Shoka skill set)")

	// workspace + shared pass-throughs.
	namespace := fs.String("namespace", "", "the namespace this working directory is responsible for")
	project := fs.String("project", "", "the project this working directory is responsible for")
	environment := fs.String("environment", "", "optional connection environment the assignment uses")
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "operate at the user level instead of the current working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Phase 1 — config-setup (the connection). Skip if already configured.
	if *noConfig {
		fmt.Println("[config] skipped (--no-setup-config)")
	} else {
		configured := false
		if cfg, err := clientconfig.Load(*env); err == nil && cfg.Endpoint != "" && cfg.Token != "" {
			configured = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("[config] %w", err)
		}
		if configured && !*reconfigure {
			fmt.Printf("[config] connection already configured for environment %q; skipping (pass --reconfigure to re-auth)\n", *env)
		} else {
			authArgs := []string{"--env", *env}
			if *endpoint != "" {
				authArgs = append(authArgs, "--endpoint", *endpoint)
			}
			if *tokenFile != "" {
				authArgs = append(authArgs, "--token-file", *tokenFile)
			}
			fmt.Println("[config] configuring connection (shoka-cli auth)")
			if err := cmdAuth(authArgs); err != nil {
				return fmt.Errorf("[config] %w", err)
			}
		}
	}

	// Phase 2 — skill update + install (the skill layer). init syncs the cache and
	// installs the WHOLE Shoka skill set (the skills are required tooling, not an
	// a-la-carte catalogue); --skill narrows to specific skills for the rare
	// targeted case. It does NOT absorb the apt lifecycle (update/upgrade stay in
	// the `skill` family).
	if *noSkill {
		fmt.Println("[skill] skipped (--no-install-skill)")
	} else {
		updateArgs := []string{"--repo", *repo}
		if *ref != "" {
			updateArgs = append(updateArgs, "--ref", *ref)
		}
		fmt.Println("[skill] syncing skills cache (shoka-cli skill update)")
		if err := cmdSkillUpdate(updateArgs); err != nil {
			return fmt.Errorf("[skill] %w", err)
		}
		// No --skill => install the whole synced set (cmdSkillInstall with no name);
		// --skill <names> => install just those.
		installArgs := []string{"--runtime", *runtime}
		if *global {
			installArgs = append(installArgs, "--global")
		}
		installArgs = append(installArgs, skills...)
		if err := cmdSkillInstall(installArgs); err != nil {
			return fmt.Errorf("[skill] install: %w", err)
		}
	}

	// Phase 3 — workspace set (the assignment). Always run unless already
	// established; --reconfigure overwrites. Not separately skippable — the
	// assignment is init's core purpose.
	base, err := conventionDir(*runtime, *global)
	if err != nil {
		return fmt.Errorf("[workspace] %w", err)
	}
	wsPath := filepath.Join(base, workspaceFileName)
	_, wsExists := statExists(wsPath)
	if wsExists && !*reconfigure {
		fmt.Printf("[workspace] assignment already established at %s; skipping (pass --reconfigure to overwrite)\n", wsPath)
	} else {
		if *namespace == "" || *project == "" {
			return fmt.Errorf("[workspace] --namespace and --project are required to establish the assignment")
		}
		wsArgs := []string{"--namespace", *namespace, "--project", *project, "--runtime", *runtime}
		if *environment != "" {
			wsArgs = append(wsArgs, "--environment", *environment)
		}
		if *global {
			wsArgs = append(wsArgs, "--global")
		}
		if *reconfigure {
			wsArgs = append(wsArgs, "--force")
		}
		if err := cmdWorkspaceSet(wsArgs); err != nil {
			return fmt.Errorf("[workspace] %w", err)
		}
	}

	fmt.Println("init complete.")
	return nil
}

// statExists reports whether path exists (any non-IsNotExist error is treated as
// "exists" so init errs toward not silently overwriting).
func statExists(path string) (os.FileInfo, bool) {
	fi, err := os.Stat(path)
	if err == nil {
		return fi, true
	}
	if os.IsNotExist(err) {
		return nil, false
	}
	return nil, true
}
