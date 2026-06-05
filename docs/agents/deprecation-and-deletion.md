---
title: Deprecation and Deletion (Agent Guide)
summary: Agent decision tree for retiring (frontmatter status) vs deleting (delete_file), with the confirmation and restoration rules.
status: active
tags: [agents, deletion, retirement, deprecation, shoka]
related:
  - docs/conventions/document-lifecycle.md
  - docs/contracts/mcp-v1.md
  - docs/operations/sensitive-data-removal.md
---

# Deprecation and Deletion (Agent Guide)

The agent-facing version of `docs/conventions/document-lifecycle.md`. Read that for
the full model; this is the decision procedure.

## Decision tree

When a document is no longer current:

1. **Does the content have future value** (a lesson, a rationale, a record)?
   - **Yes →** *retire it* with frontmatter, do not delete:
     - failed approach with a lesson → `status: failed`
       (`docs/conventions/failure-records.md`)
     - replaced but still explains the current design → `status: superseded`
       (set `related` to the replacement)
     - finished and inert → `status: archived`
   - **No →** it is a write-failure, accident, test artifact, or valueless scratch
     → `delete_file` is appropriate.
2. **When in doubt, retire rather than delete.** Retiring costs one frontmatter
   line; deleting costs a recovery round-trip.

Retiring is an ordinary `write_file` that edits frontmatter — nothing moves on
disk, and `delete_file` is not involved.

## Confirmation requirement for `delete_file`

Before any `delete_file` call, present the user a short summary containing:

- the file path (and namespace/project),
- its **current `etag`** (from `read_file`, or from `list_files include_summaries`
  where each summary carries the `etag`),
- a reminder that **Git history retains the content** and it can be restored.

Proceed only after the user confirms. (Deletion is a `git rm` + commit; see
`docs/contracts/mcp-v1.md` § 4.8.)

## Restoration (if a delete is regretted)

1. `get_history(project_name=…, path=…)` — find a commit before the delete.
2. `read_file_at_version(path=…, commit_hash=<that commit>)` — retrieve content.
3. `write_file(path=…, content=<retrieved>)` — restore as a new commit.

## Prohibition

Agents must **not** attempt history-rewriting operations (`git filter-repo`,
`git filter-branch`, force-push, etc.) through `bash` or any other channel.
Removing data from *history* is an operator procedure — see
`docs/operations/sensitive-data-removal.md`. `delete_file` only removes a file
from the current tree; it never rewrites history.

## Sources

- `docs/conventions/document-lifecycle.md` (retire-vs-delete criteria, recovery).
- `docs/contracts/mcp-v1.md` § 4.8 (`delete_file` = `git rm` + commit), § 4.6
  (`read_file_at_version`), § 7 (history preserved).
- `docs/operations/sensitive-data-removal.md` (operator-only history rewriting).
