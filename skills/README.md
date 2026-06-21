# Shoka skills — source of truth

This directory is the **tracked source-of-truth** for Shoka's "how to use Shoka"
skill set. Each subdirectory is one skill (a `SKILL.md` plus any supporting
files). These skills are required tooling installed as a **set** (not by name):
`shoka-cli skill update && shoka-cli skill install` (no name) syncs this `skills/`
subtree from the project's own public repo and installs **every** skill into a
target agent's runtime convention directory (`.claude/skills/<name>/`) — or run
`shoka-cli init`, which does the same as part of setup. `skill update` defaults to
the project repo; `--repo` overrides it. (`skill install <name>` / `skill list`
exist for targeted/visibility use.) Adding a skill here makes it install
automatically — no CLI change.

Skills are **not** Shoka data: they live in the repo tree and are version
controlled with ordinary `git`. They are never stored or fetched through the MCP
data path (`write_file`/`read_file`) and the markdown/json/yaml ingest allowlist
never applies to them.

## The per-working-dir workspace JSON (the agent assignment)

The single biggest obstacle to an agent using Shoka is that it does not know
**which namespace/project it is responsible for**. That assignment lives in a
per-working-directory **workspace JSON**, placed in the agent runtime's
convention directory:

- **Claude Code:** `.claude/shoka-workspace.json`
- **gemini-cli:** `.gemini/shoka-workspace.json`

Shape (minimal):

```json
{
  "namespace": "<namespace>",
  "project": "<project>",
  "environment": "<optional: the clientconfig environment name>"
}
```

- `namespace`, `project` — which Shoka namespace/project this working directory
  is responsible for. Required.
- `environment` — optional; names the `internal/clientconfig` environment (the
  connection: endpoint + token) this assignment should connect through. Omit to
  use the default environment.

It is **per-working-directory** (like `.git/config` is per-repo): each working
directory declares its own assignment. Skills that operate on Shoka read this
file for their namespace/project instead of hard-coding it; if it is absent, the
agent runs the initial-setup guidance skill (`shoka-workspace-setup`, B-15 step
c — not built yet) or asks the user, rather than assuming a default.

### Three distinct layers — do not conflate

| Layer | What it holds | Scope | Where |
|-------|---------------|-------|-------|
| **Connection** | endpoint + token | user-level, per-environment | `os.UserConfigDir()/shoka/<env>/config.yaml` (`internal/clientconfig`) |
| **CLI ergonomics** | `default_namespace` / `default_project` | user-level, per-environment | same `config.yaml` — defaults for the human operator's `shoka-cli file add` relative-dest resolution |
| **Agent assignment** | `namespace` / `project` (+ optional `environment`) | **per-working-dir** | the **workspace JSON** in the runtime convention dir |

The workspace JSON is the new, separate **assignment** layer. It is not a reuse
of the CLI client config's connection fields, nor of its `default_namespace`/
`default_project` ergonomics (those answer "where does the *human operator's
CLI* write by default", a different question from "which ns/project is *this
agent in this working dir* responsible for").

There is **no Go reader** of the workspace JSON in this step — the **agent** is
the reader, by following the skill prose.

## Skills here

- **`shoka-directive-onboarding/`** — the prototype "how to use Shoka" skill:
  fetch and execute the latest directive from Shoka, writing progress reports at
  decision points. Reads its namespace/project from the workspace JSON.
