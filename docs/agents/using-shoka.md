---
title: Using Shoka (Agent Guide)
summary: Practical patterns and pitfalls for an agent calling Shoka's MCP tools. Short and practical; the contract has the full detail.
status: active
tags: [agents, patterns, pitfalls, shoka]
related:
  - docs/contracts/mcp-v1.md
  - docs/agents/deprecation-and-deletion.md
  - docs/conventions/document-lifecycle.md
  - docs/conventions/failure-records.md
---

# Using Shoka (Agent Guide)

Practical guidance. For exact schemas and semantics, see `docs/contracts/mcp-v1.md`.

## Idiomatic patterns

1. **Safe write (read-modify-write):** `read_file` to get `version` → make your
   change → `write_file` with `expected_version=<that version>`. On conflict,
   re-read and retry (see § 5 of the contract).
2. **Blind write (create or intentional overwrite):** `write_file` with no
   `expected_version` (or `""`). Use only when you know you are authoritative.
3. **Cheap overview:** `list_files(include_summaries=true)` to assemble titles +
   frontmatter for many files in one call, then `read_file` only what you need.
   Use `read_summary` for a single file when you want metadata + excerpt, not body.
4. **Find prior work / failures:** `search_files(query=…, search_in="both")`.
   This reaches retired and `status: failed` documents.
5. **Recover lost content:** `get_history(path=…)` to find a commit, then
   `read_file_at_version(path=…, commit_hash=…)`, then `write_file` to restore.

## Pitfalls

1. **Do not delete documents with learning value.** Mark `status: failed` (see
   `docs/conventions/failure-records.md`) instead of `delete_file`.
2. **Do not force-overwrite on conflict without asking.** A conflict means
   someone/something else wrote since you read. Re-read; ask the user before a
   blind overwrite (`expected_version=""`).
3. **Do not assume webhook side effects happened before your call returned.**
   Delivery is async and best-effort; the write succeeds even if every webhook
   target is down.
4. **Do not put bearer tokens in URLs for MCP.** The MCP endpoint accepts only the
   `Authorization: Bearer` header; `?token=` works on WebSocket paths only.
5. **Do not use absolute paths or `..`.** Paths are project-relative; traversal
   and absolute paths are rejected.
6. **Do not expect `list_files` to hide retired documents.** It returns every
   file; filtering by `status` is your responsibility (client-side convention).
7. **Do not attempt history rewriting** (`git filter-repo`, etc.) via `bash` or
   any side channel. It is operator-only — see
   `docs/operations/sensitive-data-removal.md`.
8. **Do not invent a `namespace` when you mean the default.** Omit it; it defaults
   to `"default"`. Names must be `[A-Za-z0-9_-]+`.
9. **Treat `version` as opaque.** It is a Git commit hash; compare for equality,
   do not parse it.
10. **`read_summary` never returns the body.** If you need full content, call
    `read_file`.

## When to ask the user vs. proceed

- **Proceed:** reads; first-time creates; safe read-modify-write that does not
  conflict.
- **Ask first:** any `delete_file` (summarize what will be deleted and its current
  version first — see `docs/agents/deprecation-and-deletion.md`); any blind
  overwrite (`expected_version=""`) of a file you did not just create; anything
  that would discard another writer's change after a conflict.

## Sources

- `docs/contracts/mcp-v1.md` (§ 4 tools, § 5 locking, § 6 webhooks, § 3 auth).
- `docs/conventions/document-lifecycle.md`, `docs/conventions/failure-records.md`,
  `docs/agents/deprecation-and-deletion.md`. All behavior here is restated from
  these; no new claims.
