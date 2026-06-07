// Command shoka-cli is the Shoka maintenance CLI for humans and agents (B-46b
// foundation). It is a SEPARATE binary from the server (cmd/server) — deliberately
// so: a same-named/different-behaviour binary risks accidentally starting the
// server, so the maintenance tool is kept distinct. Building or running shoka-cli
// never starts a server.
//
// shoka-cli is a THIN MCP client: it connects to a Shoka MCP endpoint with a
// Bearer token from the local client config and calls tools. It carries NO
// Shoka-specific judgement — all ingest/format/catalog logic lives in the
// server-side tools; the client only invokes them.
//
// Subcommands:
//
//	shoka-cli auth      Store the display-once access token into the client config.
//	shoka-cli projects  Connect with the stored token and list projects (a
//	                    read-only credential check / smoke test).
//	shoka-cli file      Byte-faithful ingest of a local file (file add).
//	shoka-cli skill     apt-model skill distribution: update/install/upgrade/
//	                    outdated. git + filesystem only; no connection, no data
//	                    path.
//	shoka-cli workspace set  Write the per-working-dir workspace JSON (the agent
//	                    assignment: namespace/project) — the single write point.
//	shoka-cli init      Orchestrator: run config-setup + skill install + workspace
//	                    set by composing the per-phase subcommands (git init-like).
//
// The subcommand surface is small, so it stays on the repo's stdlib `flag`
// convention (cmd/server uses it too) with hand-rolled dispatch — no
// subcommand-library dependency until the tree is deep enough to actually need
// one.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shoka-cli: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "auth":
		return cmdAuth(args[1:])
	case "projects":
		return cmdProjects(args[1:])
	case "file":
		return cmdFile(args[1:])
	case "skill":
		return cmdSkill(args[1:])
	case "workspace":
		return cmdWorkspace(args[1:])
	case "init":
		return cmdInit(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `shoka-cli — Shoka maintenance CLI (thin MCP client)

Usage:
  shoka-cli auth      [--env NAME] [--endpoint URL] [--token-file PATH] \
                      [--default-namespace NS] [--default-project PROJ]
  shoka-cli projects  [--env NAME]
  shoka-cli file add  [--env NAME] [--namespace NS] [--project PROJ] \
                      <local-path> <dest>
  shoka-cli skill update    --repo URL-OR-PATH [--ref REF]
  shoka-cli skill install   [--runtime claude|gemini] [--global] <name>
  shoka-cli skill upgrade   [--runtime claude|gemini] [--global]
  shoka-cli skill outdated  [--runtime claude|gemini] [--global]
  shoka-cli workspace set   --namespace NS --project PROJ [--environment E] \
                            [--runtime claude|gemini] [--global] [--force]
  shoka-cli init            [--no-setup-config] [--no-install-skill] [--reconfigure] \
                            [--env NAME] [--endpoint URL] [--token-file PATH] \
                            [--repo URL-OR-PATH] [--ref REF] [--skill NAME ...] \
                            --namespace NS --project PROJ [--environment E] \
                            [--runtime claude|gemini] [--global]

The access token is read from --token-file or stdin (never from the command line,
which would leak it into shell history) and stored at
  <user-config-dir>/shoka/<env>/config.yaml  (file 0600, dir 0700).

A <dest> is a Shoka address: a relative in-project path (e.g. notes/foo.md) uses
the namespace/project from --namespace/--project or the config defaults; an
absolute /namespace/project/path names the project explicitly.

skill distribution follows the apt model: "skill update" (the one network op)
git-fetches the --repo skills repository into a local cache
(<user-cache-dir>/shoka/skills); "skill install"/"upgrade" copy from that cache
into the runtime convention dir (.claude/skills or .gemini/skills, or the
user-level dir with --global) and work offline. Skills are not Shoka data and
install never writes the workspace JSON.

"workspace set" is the single write point for the per-working-dir workspace JSON
(.claude/shoka-workspace.json or .gemini/...), the agent assignment of which
namespace/project this directory owns. It writes only the assignment — never the
connection (endpoint+token) or skill files. It refuses to overwrite an existing
assignment unless --force.

"init" is a git init-like orchestrator that runs config-setup (auth), skill
update+install (the onboarding+setup pair by default; add more with --skill), and
workspace set, by composing those subcommands. Already-established phases are
reported and skipped; --reconfigure forces them. --no-setup-config / --no-install-skill
skip a phase. It adds no logic of its own — each phase stays independently callable.
`)
}
