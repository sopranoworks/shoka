package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// workspaceFileName is the per-working-dir workspace JSON the agent reads to learn
// its namespace/project ASSIGNMENT. It lives in the runtime convention dir
// (.claude/shoka-workspace.json, .gemini/...). This is the ONE place its name +
// shape are defined; the setup skill no longer hand-writes it (B-15e).
const workspaceFileName = "shoka-workspace.json"

// workspaceConfig is the workspace JSON shape (the agent assignment). It is NOT the
// connection (endpoint+token, internal/clientconfig) nor the CLI-ergonomics defaults
// — only which namespace/project this working directory is responsible for, and
// optionally which connection environment that assignment uses.
type workspaceConfig struct {
	Namespace   string `json:"namespace"`
	Project     string `json:"project"`
	Environment string `json:"environment,omitempty"`
}

// cmdWorkspace dispatches the `workspace` subcommand group. Today it has one member,
// `set` — the single non-interactive WRITE POINT for the workspace JSON (used by the
// setup skill, an automated launcher, and a human alike).
func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("workspace: a subcommand is required (set)")
	}
	switch args[0] {
	case "set":
		return cmdWorkspaceSet(args[1:])
	default:
		return fmt.Errorf("unknown workspace subcommand %q (expected: set)", args[0])
	}
}

// cmdWorkspaceSet writes the per-working-dir workspace JSON (the agent assignment)
// into the runtime convention dir. It is the consolidated write point: the JSON
// shape and the convention-path resolution live ONLY here, so the setup skill,
// launchers, and humans all share one writer.
//
// It writes ONLY the workspace JSON — never the connection config (endpoint+token)
// or skill files. It is non-interactive (flags only). Existing-JSON behaviour
// (operator-confirmed): refuse to overwrite unless --force; with --force, overwrite
// and print old→new so the change is never silent.
func cmdWorkspaceSet(args []string) error {
	fs := flag.NewFlagSet("workspace set", flag.ContinueOnError)
	namespace := fs.String("namespace", "", "the namespace this working directory is responsible for (required)")
	project := fs.String("project", "", "the project this working directory is responsible for (required)")
	environment := fs.String("environment", "", "optional connection environment (clientconfig env) this assignment uses")
	runtime := fs.String("runtime", "claude", "target agent runtime: claude | gemini")
	global := fs.Bool("global", false, "write at the user level instead of the current working directory")
	force := fs.Bool("force", false, "overwrite an existing workspace JSON (prints the old→new change)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *namespace == "" || *project == "" {
		return fmt.Errorf("workspace set requires --namespace and --project")
	}

	base, err := conventionDir(*runtime, *global)
	if err != nil {
		return err
	}
	path := filepath.Join(base, workspaceFileName)

	// Existing-JSON behaviour: error unless --force; report old→new on overwrite.
	old, existed, err := readWorkspace(path)
	if err != nil {
		return err
	}
	if existed && !*force {
		return fmt.Errorf("workspace JSON already exists at %s; pass --force to overwrite it", path)
	}

	next := workspaceConfig{Namespace: *namespace, Project: *project, Environment: *environment}
	if err := writeWorkspace(path, next); err != nil {
		return err
	}

	if existed {
		fmt.Printf("updated workspace assignment at %s\n", path)
		fmt.Printf("  old: %s\n", formatWorkspace(old))
		fmt.Printf("  new: %s\n", formatWorkspace(next))
	} else {
		fmt.Printf("wrote workspace assignment at %s\n", path)
		fmt.Printf("  %s\n", formatWorkspace(next))
	}
	return nil
}

// readWorkspace loads the workspace JSON at path. The bool is false when no file
// exists (not an error — that is the common first-write case).
func readWorkspace(path string) (workspaceConfig, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return workspaceConfig{}, false, nil
		}
		return workspaceConfig{}, false, fmt.Errorf("read workspace JSON %s: %w", path, err)
	}
	var cfg workspaceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return workspaceConfig{}, false, fmt.Errorf("parse workspace JSON %s: %w", path, err)
	}
	return cfg, true, nil
}

// writeWorkspace writes cfg as the workspace JSON at path, creating the convention
// dir if needed. The write is atomic (temp file in the same dir, then rename) so a
// crash mid-write never leaves a truncated assignment. The workspace JSON is not a
// secret (it holds only ns/project), so ordinary 0755/0644 perms apply — unlike the
// token-bearing clientconfig.
func writeWorkspace(path string, cfg workspaceConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create convention dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workspace JSON: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".shoka-workspace-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp workspace JSON: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp workspace JSON: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp workspace JSON: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install workspace JSON %s: %w", path, err)
	}
	return nil
}

// formatWorkspace renders an assignment compactly for the old→new report.
func formatWorkspace(cfg workspaceConfig) string {
	s := cfg.Namespace + "/" + cfg.Project
	if cfg.Environment != "" {
		s += " (environment: " + cfg.Environment + ")"
	}
	return s
}
