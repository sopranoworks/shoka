---
title: Sensitive Data Removal (History Rewriting)
summary: Operator-only procedure for purging secrets/PII from Git history. Not an MCP tool, and why.
status: active
tags: [operations, security, history, git, shoka]
related:
  - docs/contracts/mcp-v1.md
  - docs/OPERATIONS.md
  - docs/agents/deprecation-and-deletion.md
---

# Sensitive Data Removal (History Rewriting)

**Scope.** This procedure removes data from a project's **Git history**, not just
from its current tree. Use it only when secrets, credentials, or PII have been
committed and must not remain recoverable. For ordinary removal of a current file
(content that may remain in history), use `delete_file` — see
`docs/conventions/document-lifecycle.md`.

## Why this is not an MCP tool

History rewriting is deliberately **not** exposed through MCP. It is destructive in
ways that break Shoka's core contract:

- It **invalidates every cached `expected_version`** — version hashes change for
  rewritten commits, so every client holding a hash gets spurious conflicts
  (optimistic locking is built on commit hashes; see
  `docs/contracts/mcp-v1.md` § Optimistic locking).
- It **breaks every existing clone** — collaborators and backups must re-clone.
- It requires human judgment (which commits, which paths, which replacement
  text) that should not be delegated to an automated client.

Agents must **not** attempt history rewriting through `bash` or any other channel
(see `docs/agents/deprecation-and-deletion.md`).

## Procedure (operator)

A project is a normal Git repository at `<base_dir>/<namespace>/<project>` (see
`docs/contracts/mcp-v1.md` § Storage model), so standard Git history-rewriting
tools apply.

1. **Stop Shoka.** Rewriting while the server may commit risks a corrupted or
   racing worktree.
2. **Rewrite history.** Use [`git filter-repo`](https://github.com/newren/git-filter-repo)
   (recommended) — for example to purge a path or replace secret text — or
   `git filter-branch` (legacy) per its documented procedure. This document does
   not reproduce those tools' manuals; follow upstream documentation.
3. **Restart Shoka.**
4. **Notify external clients.** Any client holding cached version hashes
   (Orchestrator, Cloud Run workers, an open window) must discard them: their
   `expected_version` values now refer to commits that no longer exist. Have them
   re-`read_file` to obtain fresh versions.

## Also rotate the secret

Removing a secret from history does not un-leak it. If a credential was exposed,
**rotate it** regardless of the rewrite.

## Sources

- Source: `internal/storage/fs_git.go:64-118` (each project is its own
  `git.PlainInit` repository under `<base_dir>/<namespace>/<project>`),
  `internal/storage/fs_git.go:493-516` (version = commit hash, the basis of
  optimistic locking).
- External: `git filter-repo` and `git filter-branch` upstream documentation.
- Related: `docs/contracts/mcp-v1.md`, `docs/conventions/document-lifecycle.md`.
