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
| `status`  | enum            | yes      | One of `draft`, `active`, `completed`, `archived`. |
| `tags`    | list of strings | no       | Free-form labels for discovery. |
| `related` | list of paths   | no       | Other files (project-relative paths) this document relates to. |

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
