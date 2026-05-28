---
title: Document Lifecycle (Two-Layer Model)
summary: How Shoka documents move between active and retired states via the status frontmatter field, and when to delete vs retire.
status: active
tags: [convention, lifecycle, status, retirement, shoka]
related:
  - docs/conventions/frontmatter.md
  - docs/conventions/failure-records.md
  - docs/agents/deprecation-and-deletion.md
  - docs/contracts/mcp-v1.md
---

# Document Lifecycle (Two-Layer Model)

Shoka never hides data and never (through MCP) rewrites history. A document's
"layer" is therefore not a storage location — every document lives in the same
`<base_dir>/<namespace>/<project>/<path>` tree — but a **state expressed in
frontmatter** plus a **client-side display convention**.

## The two layers

- **Active layer** — documents that are current. They have `status: draft` or
  `status: active`. Default overview views (a client calling `list_files` /
  `list_files(include_summaries=true)`) show these.
- **Retired layer** — documents that are no longer current but are *kept on
  purpose*. They have `status: superseded`, `status: failed`, or
  `status: archived`. By convention a client filters these out of default views
  (the server does not filter — `list_files` returns every file regardless of
  status; see `docs/contracts/mcp-v1.md` § Tool catalog).

Retirement is a **frontmatter edit**, performed with an ordinary `write_file`.
Nothing moves on disk.

## The `status` enum

`status` is the single authoritative lifecycle field. It is advisory (the server
does not enforce it — see `docs/conventions/frontmatter.md`); clients and Skills
agree to honor it.

| Value | Layer | Meaning | Choose when |
|-------|-------|---------|-------------|
| `draft` | active | Work in progress, not yet authoritative. | The document is being written or is provisional. |
| `active` | active | Current and authoritative. | The document is the present source of truth. |
| `superseded` | retired | Replaced by a newer document; kept for continuity. | A newer document took its place but the history/rationale still has value. Point to the replacement via `related`. |
| `failed` | retired | Records an approach that did not work. | The document captures a failure worth learning from. Use the schema in `docs/conventions/failure-records.md`. |
| `archived` | retired | Complete and frozen; no longer current but worth keeping. | The work is finished and inert (e.g. a closed milestone, a historical record). |

> Note: an earlier revision of `docs/conventions/frontmatter.md` listed
> `completed` instead of `superseded`/`failed`. Treat finished-and-frozen work as
> `archived`; this table is the current enum.

## Retire (frontmatter) vs delete (`delete_file`)

`delete_file` performs a `git rm` + commit (forward-only; content remains
recoverable from history — see below), confirmed in
`internal/storage/fs_git.go:268-335`. Retirement only changes frontmatter. Choose
between them at **retirement time**, by asking whether the content has future
value:

**Retire with frontmatter when the content should remain discoverable:**
- Failures with learning value → `status: failed` (so `search_files` finds them).
- Superseded designs that still explain *why* the current design exists →
  `status: superseded`, with `related` pointing at the replacement.
- Design-history / decision records → `status: archived`.

**`delete_file` (`git rm`) when the content has no ongoing value:**
- Write-failures and accidents (wrong path, corrupted content).
- Test artifacts and scratch files.
- Content with no learning value and no continuity need.

**Rule of thumb:** the decision is made at *retirement* time, not at *retention*
time. **When in doubt, retire rather than delete** — a retired document costs a
frontmatter line; a deleted one costs a `read_file_at_version` round-trip to
recover.

## Recovering a deleted file

`delete_file` is not destructive to history. To recover:

1. `get_history(project_name=…, path=…)` — the delete commit and all prior
   commits are listed; the content lived at the parent of the delete commit.
2. `read_file_at_version(path=…, commit_hash=<a commit before the delete>)` —
   returns the content as it was at that commit.
3. `write_file(path=…, content=<recovered>)` — restores it as a new commit.

This is not part of the normal workflow, but it is always available because
history is preserved indefinitely (see `docs/contracts/mcp-v1.md` § Storage
model). History *rewriting* — which would make recovery impossible — is out of
scope for MCP; see `docs/operations/sensitive-data-removal.md`.

## Sources

- Source: `internal/storage/fs_git.go:121-204` (write = atomic commit),
  `internal/storage/fs_git.go:256-335` (delete = `git rm` + commit),
  `internal/storage/fs_git.go:430-471` (`ReadFileAtVersion`),
  `internal/storage/fs_git.go:337-374` (`list_files` returns all files; no
  status filtering).
- Convention: `docs/conventions/frontmatter.md` (status is advisory, not enforced).
- Related: `docs/conventions/failure-records.md`, `docs/contracts/mcp-v1.md`.
