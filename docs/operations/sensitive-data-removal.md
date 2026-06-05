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

History rewriting is deliberately **not** exposed through MCP. It is a repository-wide,
irreversible operation that falls outside Shoka's per-document CRUD contract:

- It **rewrites shared Git history** — commit hashes change and **every existing
  clone, backup, and mirror is invalidated**; collaborators must re-clone. This is
  not a per-document edit.
- It requires human judgment (which commits, which paths, which replacement
  text) that should not be delegated to an automated client.

Note that this does **not** disturb optimistic locking. The lock token is a file's
`etag` — the SHA-256 of its **content** — not a commit hash (see
`docs/contracts/mcp-v1.md` § Optimistic locking). A history rewrite changes commit
hashes but not file content, so cached etags remain valid. The only stale references
are cached **commit hashes** used for historical reads (`read_file_at_version`,
`get_history`), which point at commits that no longer exist.

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
4. **Refresh cached commit references.** Optimistic-locking tokens are unaffected —
   an `etag` is a content hash and survives the rewrite. But any client holding a
   **commit hash** for historical reads (`read_file_at_version`, `get_history`) —
   Orchestrator, Cloud Run workers, an open window — now references a commit that no
   longer exists; have it obtain fresh hashes via `get_history`.

## Also rotate the secret

Removing a secret from history does not un-leak it. The sensitive objects may still
exist in **clones, forks, backups, mirrors, and caches** beyond the repository you
rewrote. If a credential was exposed, **rotate it** regardless of the rewrite — this,
not any locking concern, is why rotation is mandatory.

## Sources

- Source: `internal/storage/fs_git.go` (each project is its own `git.PlainInit`
  repository under `<base_dir>/<namespace>/<project>`; the `etag` returned by
  `read_file`/`write_file` is the SHA-256 of file content — the optimistic-locking
  token — while a Git commit hash is a separate identifier used only by
  `read_file_at_version`/`get_history`).
- External: `git filter-repo` and `git filter-branch` upstream documentation.
- Related: `docs/contracts/mcp-v1.md`, `docs/conventions/document-lifecycle.md`.
