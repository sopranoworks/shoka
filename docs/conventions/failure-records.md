---
title: Failure Records
summary: Frontmatter schema and conventions for status:failed documents so failed approaches are searchable and reusable.
status: active
tags: [convention, failure, learning, search, shoka]
related:
  - docs/conventions/document-lifecycle.md
  - docs/conventions/frontmatter.md
  - docs/contracts/mcp-v1.md
---

# Failure Records

A failure record is a retired document (`status: failed`, see
`docs/conventions/document-lifecycle.md`) that captures an approach that did not
work, so a future agent searching for the same problem finds the dead end instead
of re-walking it.

## Frontmatter schema

In addition to the base fields (`title`, `summary`, `status`, see
`docs/conventions/frontmatter.md`), a `status: failed` document carries:

| Field | Type | Meaning |
|-------|------|---------|
| `failure_mode` | string | What went wrong, observably (the symptom). |
| `root_cause` | string | Best current understanding of *why* it failed. |
| `keywords` | list of strings | Terms that make the record findable via `search_files` (error strings, API names, symptoms). |
| `alternative` | string (path) | Project-relative path to a working approach, if one exists. |
| `attempted_at` | string (date) | When the approach was attempted (YYYY-MM-DD). |

Example:

```markdown
---
title: SSE keep-alive via raw flusher
summary: Tried manual flushing for SSE; superseded by the SDK's handler.
status: failed
failure_mode: Client saw no events; stream appeared open but empty.
root_cause: Manual http.Flusher races the SDK's own writer; double-managed stream.
keywords: [sse, flush, keep-alive, no events, empty stream]
alternative: specs/sse-via-sdk-handler.md
attempted_at: 2026-05-20
---

# SSE keep-alive via raw flusher
...
```

## File naming

Recommended: `<project>/failures/<task-id>-<short-name>.md` (e.g.
`failures/T123-sse-raw-flush.md`). Any path is acceptable as long as the
frontmatter is correct — discovery is driven by frontmatter and content, not
location.

## How agents find failure records

Failure records are in the retired layer, so default overview views hide them. To
search them deliberately:

- `search_files(project_name=…, query=<keyword>, search_in="both")` returns
  filename and content matches with context snippets (see
  `docs/contracts/mcp-v1.md` § Tool catalog → `search_files`). `search_files`
  does **not** filter by status, so it reaches retired records.
- Client-side overview filters that hide `status: failed` must **opt in** to
  including failures when the user is explicitly searching for prior failures.

## Coalescing recurring failures

If a new failure closely matches an existing record, **append an `## Occurrence`
section to the existing file** rather than creating a near-duplicate. The direct way
is `append_to_file` (`content="\n## Occurrence …"`, `position="end"`, or
`position="after"` with a unique `anchor`), which adds the section without resending
the whole file; if you instead read-modify-write the file, pass the `etag` from
`read_file` as `if_match` on the `write_file`. Each occurrence section notes its own
`attempted_at` and any new detail. This keeps one searchable record per failure mode
instead of many partial ones.

## Sources

- Convention: `docs/conventions/document-lifecycle.md` (`status: failed` is a
  retired layer state), `docs/conventions/frontmatter.md` (base fields; status
  advisory).
- Source: `internal/tools/discovery.go:52-91` and `internal/storage/discovery.go:146-205`
  (`search_files` is case-insensitive substring over filename/content/both with
  snippets; no status filtering).
