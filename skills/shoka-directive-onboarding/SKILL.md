---
name: shoka-directive-onboarding
description: Use when the user asks to "execute the latest directive" or "read latest instructions from Shoka". Fetches the most recent directive document from the Shoka MCP server, presents it for confirmation, then executes it. Writes progress reports to Shoka at decision points so intermediate thinking is preserved.
---

# Shoka Directive Onboarding

This skill is project-local: each working directory it runs in is responsible
for one Shoka **namespace/project**. The skill does **not** hard-code which one
— it reads the assignment from a per-working-dir **workspace JSON**.

## Workspace: which namespace/project this agent is responsible for

Before doing anything else, learn your namespace/project from the **workspace
JSON** in this runtime's convention directory:

- **Claude Code:** `.claude/shoka-workspace.json`
- **gemini-cli:** `.gemini/shoka-workspace.json`

Shape:

```json
{
  "namespace": "<namespace>",
  "project": "<project>",
  "environment": "<optional: the clientconfig environment name>"
}
```

Use the `namespace` and `project` from that file everywhere this skill says
"the configured namespace/project" (or "this namespace/project").

**If the workspace JSON is absent**, do not guess and do not fall back to any
hard-coded default. Run the **initial-setup guidance skill**
(`shoka-workspace-setup`), which walks the user through choosing the
namespace/project and writes the workspace JSON; then re-read it and continue.
(That setup skill is a separate skill — installed alongside this one. If it is
not present, tell the user the workspace JSON is missing and ask which
namespace/project this working directory is responsible for; do not proceed
without an answer.)

The workspace JSON is the agent **assignment** (which ns/project this working
dir owns) and is distinct from the CLI client config
(`os.UserConfigDir()/shoka/<env>/config.yaml`), which holds the **connection**
(endpoint + token) plus the CLI's own optional default-ns/project ergonomics:
connection vs assignment, user-level vs per-working-dir.

## CRITICAL: Source of truth

**The directive you are to execute lives in Shoka. Not in your memory, not in
your training data, not in your prior turn's context.** Every time the user
says "execute the latest directive" — or any phrasing of that kind — you
**must** start by reading from Shoka, even if you remember executing a
similar-sounding directive previously or believe you already know what the
"latest" directive must be.

Specifically:

- **Do not assume a directive has already been completed.** The operator may
  have placed a new one with a title you remember from a previous session.
- **Do not assume the directives folder is empty.** Verify by listing.
- **Do not skip the read step** even if you "already know" the directive's
  contents from a prior turn or previous session.
- **If you find no active directive, stop and ask the operator.** Do not
  proceed with "self-directed cleanup" or any other inferred task. The skill's
  whole purpose is the directive — without one, there is no task.

This rule exists because directives change between sessions and your memory
of "what was completed" is unreliable. The cost of re-reading is one MCP call;
the cost of acting on a stale memory is a wasted Coder session and corrupted
project state.

## Workflow

1. **Always begin by listing directives via Shoka MCP.** Use `list_files` with
   `path: "directives/"` on the configured namespace/project. If no Shoka MCP
   server is connected, tell the user and stop — do not invent a task.
2. **Identify the latest directive.** Order entries by filename. Among
   entries with the same date prefix, the lexicographically largest filename
   is the latest by convention (note: this convention is imperfect; if you
   are unsure, ASK rather than guess). If a directive has a completion
   report under `reports/` named with a matching slug, that directive is
   already completed — pick the latest one **without** a matching completion
   report.
3. **If you cannot uniquely identify the latest active directive** (e.g.
   multiple candidates, all incomplete; ambiguous naming), **stop and ask
   the operator** which to execute. Do not pick one arbitrarily.
4. **If the candidate set is empty** (every directive in `directives/` has
   a matching completion report under `reports/`), **stop and tell the
   operator there is no active directive**. Do not proceed with
   self-directed work.
5. **Read the chosen directive in full.** Use `read_file` on the chosen
   directive's full path. Read it completely; do not summarise from the
   filename or your prior memory of similar-named directives.
6. **Read all documents listed in `related:`** that resolve to paths in this
   namespace/project. Use `read_file` for each. (Paths starting with `../`
   may resolve outside the project; for those, ask the operator how to
   access them rather than guessing a filesystem path.)
7. **Present a concise summary to the operator** before starting:
   - directive title (from frontmatter)
   - what it asks to be done (one paragraph)
   - related documents that were read
   - any explicit "completion criteria" or "non-negotiable constraints"
   - any uncertainty you have about the directive itself
8. **Wait for explicit confirmation** before starting work. The operator may
   correct your understanding of the directive or override its instructions.
9. **Execute the directive.** Follow it strictly. Do not paraphrase its
   constraints into looser forms.
10. **Write progress reports at decision points** (see "Progress reports"
    below).
11. **When complete, write a final completion report** to the path the
    directive specifies (or to
    `reports/<date>-<directive-slug>-complete.md` if the directive does not
    name a path), using Shoka's `write_file`. The report structure follows
    what the directive specifies; if unspecified, use a default structure
    with frontmatter (`title`, `summary`, `status: active`, `tags`,
    `related`) and sections covering: outcome, what was done, tests run,
    deviations.

## Forbidden behaviours

The following behaviours have caused real problems in previous sessions and
are explicitly forbidden:

- **Skipping the directive-listing step** because you believe you remember
  what the latest directive is.
- **Inferring that there is no active directive** without first listing the
  `directives/` folder via MCP and cross-checking against `reports/`.
- **Proceeding with self-chosen "useful cleanup work"** when no directive is
  available. The skill governs directive-driven work; outside that scope,
  the answer is "ask the operator".
- **Reading the directive once, then re-using the contents from a prior
  session's memory in a later session.** Every session re-reads. Directives
  may have been updated.
- **Picking among multiple ambiguous candidates** without asking.
- **Using local filesystem `cat` to read a directive that should be read
  via MCP.** The MCP path is the audit-able one; the local path may diverge
  silently. Read via MCP first. Fall back to local filesystem only if MCP
  is unreachable AND the operator has authorised the fallback in this session.

## Progress reports

Mid-execution thinking that is non-trivial — a plan ready for approval, a
research finding that changes direction, a design decision, a deviation
that needs to be recorded, an agreement reached with the operator — should
be written to Shoka as a progress report **before continuing**.

This exists because conversational outputs (plans, investigations,
trade-off discussions) otherwise live only in the chat log and are lost
when the session ends. Writing them to Shoka preserves the project's
reasoning trail alongside its directives and completion reports.

**Write a progress report when:**

- A multi-phase plan is presented for operator approval (write the plan
  itself).
- A research step has produced findings that will inform implementation
  (write what was learned and how it changes the approach).
- A design decision is made (license, library choice, deviation from
  directive, etc. — write what was chosen and why).
- The operator and the agent reach an explicit agreement that constrains
  subsequent work.

**Skip the report when:**

- Work is routine, follows the directive directly, and finishes inside
  the next few tool calls.
- It is a single-line confirmation, a build success, a passing test —
  these are captured in the final completion report.

**Test:** "If this conversation ended right now, would the project lose
information that the next agent would need?" If yes, write the report.

**Format:**

- Path: `reports/progress/<UTC-date>-<short-slug>.md`. Example:
  `reports/progress/2026-05-30-stress-test-design-decisions.md`.
- Frontmatter:
  ```yaml
  ---
  title: <short title>
  summary: <one sentence>
  status: active
  tags: [progress, <other relevant tags>]
  related:
    - directives/<the directive being executed>
  ---
  ```
- Body: what was decided / discovered / agreed, why, and what comes next.
  Keep it concise — this is a record, not a report.
- Use `write_file` to commit it. Do not block on it; if Shoka is
  unavailable, note that to the operator and continue, then write the
  report retroactively when Shoka is reachable.

## Constraints

- Do not write to Shoka outside the configured namespace/project.
- Do not delete any file under this namespace/project. The directive trail
  is append-mostly; superseded items are marked via frontmatter `status`,
  not removed.
- If the directive itself contradicts these constraints, the directive
  wins for its specific scope, but record the conflict in the completion
  report.

## Workspace bindings

The namespace and project are **not** hard-coded in this skill — they come from
the per-working-dir workspace JSON described under "Workspace" above
(`.claude/shoka-workspace.json` for Claude Code, `.gemini/shoka-workspace.json`
for gemini-cli). This makes the skill portable: the same skill content serves
any working directory, and each directory declares its own assignment. If the
workspace JSON is absent, run the `shoka-workspace-setup` skill (or ask the
user) rather than assuming a default.
