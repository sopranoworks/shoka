---
name: shoka-workspace-setup
description: Use when a working directory has no Shoka workspace JSON yet — the interactive initial-setup fallback that the shoka-directive-onboarding skill sends you to when it cannot find the per-working-dir assignment. Discovers the available namespaces/projects via the list_projects MCP tool, leads the user to choose which one this working directory is responsible for, and writes the workspace JSON. Sets only the assignment, not the connection.
---

# Shoka Workspace Setup

This skill establishes **which Shoka namespace/project this working directory's
agent is responsible for** — the agent *assignment* — by writing a per-working-dir
**workspace JSON**. It is the interactive fallback that
`shoka-directive-onboarding` (and any other Shoka skill that needs an assignment)
sends you to when no workspace JSON exists yet.

It does exactly one thing: choose an assignment and record it. It does **not**
configure the connection (endpoint + token) — that is a separate concern (see
"The three layers" below).

## When this skill runs

Run this skill when **no workspace JSON is present** in this runtime's convention
directory:

- **Claude Code:** `.claude/shoka-workspace.json`
- **gemini-cli:** `.gemini/shoka-workspace.json`

The onboarding skill triggers this automatically on absence; the user may also
invoke it directly to (re)establish the assignment.

If a workspace JSON already exists and the user only wants to *change* the
assignment, you may follow the same steps and overwrite it — but confirm with the
user first, since other skills rely on the current assignment.

## What you will produce

A single file — the **workspace JSON** — in the convention directory above, with
this exact shape (the same shape documented in `skills/README.md`; do not invent
a new one):

```json
{
  "namespace": "<namespace>",
  "project": "<project>",
  "environment": "<optional: the clientconfig environment name>"
}
```

- `namespace`, `project` — which Shoka namespace/project this working directory is
  responsible for. **Required.**
- `environment` — optional; names the connection environment (the
  `internal/clientconfig` environment holding endpoint + token) this assignment
  connects through. Omit it to use the default environment.

The values above are **abstract placeholders**. Never hard-code a concrete
namespace/project into the file from memory or example — every concrete value
comes from `list_projects` at runtime or from the user's explicit choice (see the
steps below).

## Steps

1. **Detect that no workspace JSON exists.** Check the convention directory for
   this runtime (`.claude/shoka-workspace.json` for Claude Code,
   `.gemini/shoka-workspace.json` for gemini-cli). If one already exists, see
   "When this skill runs" above before overwriting. If absent, continue.

2. **Confirm Shoka is reachable.** This skill needs the Shoka MCP server
   connected (it uses `list_projects`). If no Shoka MCP server is connected, tell
   the user — the *connection* (endpoint + token) must be configured first via
   `shoka-cli auth` (see "The three layers"). Do not guess an assignment without
   being able to list real projects.

3. **Discover the available namespaces/projects.** Call the **`list_projects`**
   MCP tool — a read; it makes no changes. With no `namespace` argument it
   returns every project across all namespaces as `"<namespace>/<project>"`
   entries; pass a `namespace` to scope to one. Present the real choices to the
   user. Do **not** guess or hard-code — the choices come from this call.

4. **Lead the user to choose the assignment.** Ask the user which
   `namespace/project` *this working directory* is responsible for, choosing from
   what `list_projects` returned.
   - If the project the user wants **does not exist yet**, point them at the
     `create_project` MCP tool (input: `namespace`, `project_name`) to create it,
     then re-run `list_projects` to confirm it appears. Creating the project is
     the user's call to make; this skill's own job is the *assignment*, so keep
     that boundary — record the assignment once the project exists.
   - If the choice is ambiguous or the user is unsure, ask rather than picking
     one arbitrarily.

5. **Write the workspace JSON.** Write the chosen values into the convention
   directory using the exact shape above. Optionally set `environment` if the
   user wants this assignment to use a specific (non-default) connection
   environment; otherwise omit it.

   **This is an ordinary file in the runtime's convention directory** (e.g.
   `.claude/shoka-workspace.json`) — write it with the normal file-writing tool.
   It is **not** Shoka data: do **not** write it through the Shoka
   `write_file`/`read_file` MCP data path, and the ingest allowlist does not
   apply to it.

6. **State the layer boundary to the user.** Tell the user that this set the
   **assignment** (which namespace/project this working directory owns), and that
   the **connection** (endpoint + token) is configured separately via
   `shoka-cli auth`. This skill does **not** set the connection — if Shoka was
   not reachable in step 2, that is what they need to configure first.

7. **Hand back.** Tell the user (or the calling skill) that the workspace JSON is
   written, and that the originating skill can now re-read it and continue. If
   `shoka-directive-onboarding` sent you here, it should re-read the workspace
   JSON and proceed with its workflow.

## The three layers — do not conflate

This skill writes only the **assignment**. Keep the three layers separate (the
full table is in `skills/README.md`):

| Layer | What it holds | Scope | Set by |
|-------|---------------|-------|--------|
| **Connection** | endpoint + token | user-level, per-environment | `shoka-cli auth` (the clientconfig `config.yaml`) |
| **CLI ergonomics** | `default_namespace` / `default_project` | user-level, per-environment | `shoka-cli` config (the human operator's CLI defaults) |
| **Agent assignment** | `namespace` / `project` (+ optional `environment`) | **per-working-dir** | **this skill** — the workspace JSON |

"Tell, don't do": this skill may *tell* the user the connection is
`shoka-cli auth`'s job, but it does **not** configure the connection or the
CLI-ergonomics defaults itself.

## Constraints

- **Discover, don't guess.** Concrete namespaces/projects come from
  `list_projects` (or the user's explicit choice), never hard-coded. Any example
  in this skill is an abstract placeholder.
- **Reuse the workspace JSON shape exactly** (`{namespace, project,
  environment?}`, in the runtime convention dir). Do not invent a new shape; it
  must match what `shoka-directive-onboarding` reads and what `skills/README.md`
  documents.
- **Assignment only.** Write the workspace JSON and nothing else: do not set the
  connection (endpoint + token) or the CLI-ergonomics defaults.
- **The workspace JSON is not Shoka data.** Write it as an ordinary file in the
  convention directory, never through the `write_file`/`read_file` MCP data path.
