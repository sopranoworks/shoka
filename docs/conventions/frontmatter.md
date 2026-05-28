---
title: Frontmatter Convention
summary: YAML frontmatter that lets overview tools reason about documents without reading their bodies. Advisory, not server-enforced.
status: active
tags: [convention, frontmatter, metadata, shoka]
related:
  - docs/conventions/document-lifecycle.md
  - docs/conventions/failure-records.md
  - docs/contracts/mcp-v1.md
---

# Frontmatter Convention

Shoka stores plain Markdown files. To let overview windows and skills reason about
documents *without reading their full bodies*, documents SHOULD begin with a YAML
frontmatter block.

This is a **convention, not a server-enforced schema**. The server reads whatever
YAML is present (and tolerates malformed or absent frontmatter — it simply returns
an empty frontmatter object). The convention exists so that tools like
`read_summary` and `list_files(include_summaries=true)` return useful, predictable
metadata.

## Format

A frontmatter block is delimited by `---` fences at the very top of the file:

```markdown
---
title: Phase 3 Concurrency Plan
summary: Optimistic locking via expected_version on write_file/delete_file.
status: active
tags: [concurrency, storage]
related:
  - specs/locking.md
---

# Phase 3 Concurrency Plan

The first paragraph becomes the excerpt...
```

## Fields

| Field     | Type            | Required | Meaning |
|-----------|-----------------|----------|---------|
| `title`   | string          | yes      | Human-readable document title. |
| `summary` | string (≤200)   | yes      | One-line summary. Keep it ≤200 characters. |
| `status`  | enum            | yes      | Lifecycle state: one of `draft`, `active`, `superseded`, `failed`, `archived`. Definitions and choice criteria live in `docs/conventions/document-lifecycle.md`. |
| `tags`    | list of strings | no       | Free-form labels for discovery. |
| `related` | list of paths   | no       | Other files (project-relative paths) this document relates to. |

`status: failed` documents carry additional fields — see
`docs/conventions/failure-records.md`.

## How Shoka uses it

- **`read_summary`** returns the parsed frontmatter, the first heading, a capped
  excerpt (≤200 runes) of the first paragraph, the file size, and the last commit
  hash/timestamp — never the full body.
- **`list_files(include_summaries=true)`** returns each file's frontmatter and
  first heading, so an overview can be assembled with a single call.

## Notes

- Excerpts are capped at 200 runes (not bytes), so multibyte content such as
  Japanese is handled correctly.
- If a file has no frontmatter, `read_summary` still returns its first heading and
  excerpt; the `frontmatter` object is simply empty.
- Malformed YAML never causes an error; it yields an empty `frontmatter` object.

## Sources

- Source: `internal/markdown/markdown.go` (frontmatter parse; tolerant of
  malformed/absent YAML; `MaxExcerptRunes = 200`), `internal/tools/summary.go:14-72`
  (`read_summary` fields), `internal/tools/project.go:88-175`
  (`list_files include_summaries`).
- Convention: `docs/conventions/document-lifecycle.md` (the `status` enum),
  `docs/conventions/failure-records.md` (`status: failed` extra fields).
