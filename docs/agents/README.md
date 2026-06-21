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

## Getting the Shoka skills (`shoka-cli skill`)

Shoka ships agent skills — guidance an agent loads to use Shoka well — in its own
`skills/` directory: **`shoka-directive-onboarding`** (fetch and execute the latest
directive) and **`shoka-workspace-setup`** (the interactive first-run that records
which namespace/project a working directory owns). Install them with the
maintenance CLI:

```sh
shoka-cli skill update                              # sync the skills cache
shoka-cli skill install shoka-directive-onboarding  # place into .claude/skills/<name>/
shoka-cli skill install shoka-workspace-setup
```

- `skill update` syncs from the **project's own public repo by default**
  (`github.com/sopranoworks/shoka`, its `skills/` subtree) — no repo URL to supply.
  Pass `--repo <url-or-path>` only to override the source (e.g. a fork or a local
  checkout). It fetches just the `skills/` subtree, not the whole repository.
- `skill install <name>` copies a cached skill into the runtime convention dir —
  `.claude/skills/<name>/` (Claude Code) or, with `--runtime gemini`,
  `.gemini/skills/<name>/`; `--global` installs at the user level. It places skill
  files only; it does **not** write the workspace JSON (the assignment) — that is
  `shoka-workspace-setup`'s job.
- `skill outdated` / `skill upgrade` show and re-apply skills whose cached content
  has changed.

The skills are distributed by a runtime clone of the public repo, **not** bundled
in the `.deb` — installing Shoka does not place them; run `skill install` to get them.

## Sources

- `docs/contracts/mcp-v1.md` § 4.0 (common conventions), § 5 (locking), § 6
  (webhooks). All claims here are restated from that contract.
