---
name: shoka-workspace-setup
description: Use when a working directory has no Shoka workspace JSON yet — the interactive initial-setup fallback that the shoka-directive-onboarding skill sends you to when it cannot find the per-working-dir assignment. Discovers the available namespaces/projects via the list_projects MCP tool, leads the user to choose which one this working directory is responsible for, and records the assignment by calling shoka-cli workspace set. Sets only the assignment, not the connection.
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

The **workspace JSON** — the per-working-dir record of which Shoka
namespace/project this working directory is responsible for — written into the
convention directory above. You do **not** hand-write this file: you record the
assignment by running **`shoka-cli workspace set`** (step 5), which is the single
write point that owns the file's shape and convention-dir location.

You choose two things and pass them to that command:

- **namespace / project** — which Shoka namespace/project this working directory is
  responsible for. **Required.** Every concrete value comes from `list_projects` at
  runtime or the user's explicit choice (see the steps) — never hard-coded from
  memory or an example.
- **environment** *(optional)* — the connection environment (the `shoka-cli auth`
  clientconfig environment holding endpoint + token) this assignment uses. Omit it
  to use the default environment.

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

5. **Record the assignment with `shoka-cli workspace set`.** Run:

   ```
   shoka-cli workspace set --namespace <namespace> --project <project>
   ```

   substituting the values the user chose. Add `--environment <env>` if the user
   wants this assignment to use a specific (non-default) connection environment;
   add `--runtime gemini` if the runtime is gemini-cli (the default is Claude
   Code); add `--global` to write at the user level instead of this working
   directory.

   `workspace set` is the **single write point** for the workspace JSON — it owns
   the file's shape and convention-dir location, so you do **not** hand-write the
   file or compose its JSON yourself. If a workspace JSON already exists and the
   user wants to change it, re-run with `--force` (it prints the old→new change);
   otherwise `workspace set` refuses to overwrite.

   The workspace JSON is **not** Shoka data: `workspace set` writes an ordinary
   file in the runtime's convention directory — never through the Shoka
   `write_file`/`read_file` MCP data path, and the ingest allowlist does not apply.

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
- **Record via `shoka-cli workspace set`; do not hand-write the JSON.** The
  workspace JSON's shape and convention-dir location live in `workspace set`, the
  single write point — call it rather than composing the file yourself, so the
  interactive (this skill), automated (a launcher), and manual (a human) paths all
  produce an identical assignment.
- **Assignment only.** Set the assignment and nothing else: do not configure the
  connection (endpoint + token) or the CLI-ergonomics defaults.
- **The workspace JSON is not Shoka data.** `workspace set` writes an ordinary file
  in the convention directory, never through the `write_file`/`read_file` MCP data
  path.
