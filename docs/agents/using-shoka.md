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

1. **Safe write (read-modify-write):** `read_file` to get its `etag` → make your
   change → `write_file` with `if_match=<that etag>`. On conflict, re-read and
   retry (see § 5 of the contract).
2. **Blind write (create or intentional overwrite):** `write_file` with no
   `if_match`. Use only when you know you are authoritative.
3. **Cheap overview:** `list_files(include_summaries=true)` to assemble titles +
   frontmatter for many files in one call, then `read_file` only what you need.
   Use `read_summary` for a single file when you want metadata + excerpt, not body.
4. **Find prior work / failures:** `search_files(query=…, search_in="both")`.
   This reaches retired and `status: failed` documents.
5. **Recover lost content:** `get_history(path=…)` to find a commit, then
   `read_file_at_version(path=…, commit_hash=…)`, then `write_file` to restore.
6. **Small edit without resending the file:** `patch_file(old_string=…,
   new_string=…)` replaces **one unique** occurrence — zero or multiple matches is
   an error, so include enough surrounding context to make `old_string` unique.
   Ideal for flipping a `status:` line or fixing one paragraph in a large file.
7. **Append / insert without resending the file:** `append_to_file(content=…)`
   adds at end by default, or `position=before|after` with a **unique** `anchor`
   to insert at a spot. `content` is inserted verbatim — you own the newlines.
   Both 6 and 7 take an optional `if_match`, like `write_file`.
8. **Rename or move a file:** `move_file(source_path=…, target_path=…)` is a pure,
   history-preserving rename. It does **not** rewrite links that point at the moved
   file (`links_rewritten` is always 0) — fix any inbound links yourself.
9. **Copy a file across projects:** `copy_file(source_namespace=…,
   source_project_name=…, source_path=…, namespace=…, project_name=…)` copies a
   file to another namespace/project (or the same one at a different path). It
   **does not overwrite** — the call fails if the destination path already exists.
   The source is left unchanged (this is copy, not move). Useful for templates or
   correcting a misplaced write.

## ask_the_librarian — dual search

When the optional classifier is enabled (operator configuration), `ask_the_librarian`
runs **both** fulltext bigram search and **vector similarity search** in parallel,
merging results. This means the librarian may return documents that are semantically
related to your query even when they use completely different terminology. Without the
classifier, only fulltext search is used.

## Pitfalls

1. **Do not delete documents with learning value.** Mark `status: failed` (see
   `docs/conventions/failure-records.md`) instead of `delete_file`.
2. **Do not force-overwrite on conflict without asking.** A conflict means
   someone/something else wrote since you read. Re-read; ask the user before a
   blind overwrite (a `write_file` with no `if_match`).
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
9. **Treat `etag` as opaque.** It is the SHA-256 of the file's content (returned by
   `read_file`); compare for equality, do not parse it. It is **not** a Git commit
   hash — the commit hash is a separate token from `get_history`, used only by
   `read_file_at_version`.
10. **`read_summary` never returns the body.** If you need full content, call
    `read_file`.

## When to ask the user vs. proceed

- **Proceed:** reads; first-time creates; safe read-modify-write that does not
  conflict.
- **Ask first:** any `delete_file` (summarize what will be deleted and its current
  `etag` first — see `docs/agents/deprecation-and-deletion.md`); any blind overwrite
  (a `write_file` with no `if_match`) of a file you did not just create; anything
  that would discard another writer's change after a conflict.

## Sources

- `docs/contracts/mcp-v1.md` (§ 4 tools, § 5 locking, § 6 webhooks, § 3 auth).
- `docs/conventions/document-lifecycle.md`, `docs/conventions/failure-records.md`,
  `docs/agents/deprecation-and-deletion.md`. All behavior here is restated from
  these; no new claims.
