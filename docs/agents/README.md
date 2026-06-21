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

The Shoka skills are the **tooling an agent needs to use Shoka** — they are
installed as a **set**, not picked by name. Get them in **one step**, either as
part of first-time setup:

```sh
shoka-cli init …    # configures the connection, installs the WHOLE skill set, sets the workspace assignment
```

or, if the connection is already configured, with the two skill commands:

```sh
shoka-cli skill update     # sync the skill set from the project repo (one network op)
shoka-cli skill install     # install the WHOLE set — no skill names to know
```

You do **not** need to know individual skill names. The set is data-driven over
what the source ships (today: `shoka-directive-onboarding` — fetch/execute the
latest directive; `shoka-workspace-setup` — the interactive first-run that records
which namespace/project a working directory owns); a new skill is picked up
automatically.

- `skill update` syncs from the **project's own public repo by default**
  (`github.com/sopranoworks/shoka`, its `skills/` subtree) — no repo URL to supply;
  pass `--repo <url-or-path>` only to override (a fork or a local checkout). It
  fetches just the `skills/` subtree, not the whole repository.
- `skill install` (no name) installs **every** skill in the synced cache into the
  runtime convention dir — `.claude/skills/<name>/` (Claude Code) or, with
  `--runtime gemini`, `.gemini/skills/<name>/`; `--global` installs at the user
  level. It places skill files only; it does **not** write the workspace JSON.
  (`skill install <name> …` installs only the named skills, for the rare targeted
  case; `skill list` shows the set.)
- `skill outdated` / `skill upgrade` show and re-apply skills whose cached content
  has changed.

The skills are distributed by a runtime clone of the public repo, **not** bundled
in the `.deb` — installing Shoka does not place them; run `skill install` (or
`init`) to get them.

## Sources

- `docs/contracts/mcp-v1.md` § 4.0 (common conventions), § 5 (locking), § 6
  (webhooks). All claims here are restated from that contract.
