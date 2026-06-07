package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// conventionDir resolves an agent runtime's per-working-dir convention directory —
// the directory that holds both the installed skills (under skills/) and the
// per-working-dir workspace JSON (shoka-workspace.json):
//
//	claude -> .claude   (~/.claude with --global)
//	gemini -> .gemini   (~/.gemini with --global)
//
// --global selects the user-level location; otherwise it is relative to the
// current working directory. This is the ONE place the runtime→convention-dir
// mapping lives: skillsConventionDir (skill install/upgrade) and the workspace-JSON
// write point (workspace set) both build on it, so the path logic is never
// re-derived.
func conventionDir(runtime string, global bool) (string, error) {
	var name string
	switch runtime {
	case "claude":
		name = ".claude"
	case "gemini":
		name = ".gemini"
	default:
		return "", fmt.Errorf("unknown runtime %q (expected: claude, gemini)", runtime)
	}
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for --global: %w", err)
		}
		return filepath.Join(home, name), nil
	}
	return name, nil
}
