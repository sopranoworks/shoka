---
title: Shoka for Agents
summary: Agent entry point. Shoka is a Git-versioned Markdown store over MCP; links to the full contract and conventions.
status: active
tags: [agents, entry-point, shoka]
related:
  - docs/contracts/mcp-v1.md
  - docs/agents/using-shoka.md
  - docs/agents/deprecation-and-deletion.md
  - docs/conventions/failure-records.md
---

# Shoka for Agents

Shoka is a Markdown document store with Git versioning, exposed over MCP. You read
and write documents; Shoka tracks history. You do not manage Git directly.

## Where to look

| You need | Read |
|----------|------|
| The full MCP contract (tools, auth, errors, webhooks) | `docs/contracts/mcp-v1.md` |
| How to call tools idiomatically; pitfalls | `docs/agents/using-shoka.md` |
| Retire vs. delete a document | `docs/agents/deprecation-and-deletion.md` |
| Record/find a failed approach | `docs/conventions/failure-records.md` |
| Document states (`status`) | `docs/conventions/document-lifecycle.md` |

## Three things to know up front

1. `namespace` is optional and defaults to `"default"`.
2. On a mutating call (`write_file`/`delete_file`/`move_file`/`append_to_file`/
   `patch_file`), omitting `if_match` skips the optimistic-concurrency check; pass
   the `etag` from `read_file` to enforce it. (`etag` is the SHA-256 of the file's
   content, not a Git commit hash.)
3. Webhook deliveries are asynchronous — a successful write returns immediately and
   does not wait for (or fail on) webhook delivery.

## Sources

- `docs/contracts/mcp-v1.md` § 4.0 (common conventions), § 5 (locking), § 6
  (webhooks). All claims here are restated from that contract.
