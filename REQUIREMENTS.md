# v1 Requirements: Shoka (Markdown-MCP Backend Server)

This is the **historical record** of Shoka's v1 requirements. All v1 requirements
are implemented and validated. This document no longer describes architecture or
operations — those live in [`docs/`](docs/) and [`README.md`](README.md). Each
entry links to where its implementation is now documented.

## v1 Requirements (all complete)

| ID | Requirement | Implemented / documented in |
|----|-------------|-----------------------------|
| **META-01** | Filesystem project isolation under `<base_dir>/<namespace>/<projectName>`, enforced by name validation + path-traversal guards; no UUIDs. | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (design choices); [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 7 |
| **META-02** | Project identity is the `namespace/projectName` path itself; no metadata database (the planned SQLite UUID map was dropped — final). | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| **FILE-01** | Project creation, file management, and translation exposed as MCP tools. | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 4 |
| **FILE-02** | CRUD for Markdown files within a project. | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 4.4–4.8, and the move + partial-edit tools § 4.13–4.15 |
| **VER-01** | Every write is an atomic Git commit (`go-git`). | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 7 |
| **VER-02** | Git history exposed to agents as tools. | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 4.9 (`get_history`), § 4.6 (`read_file_at_version`) |
| **TRANS-01** | Manual, human-triggered Japanese→English translation via Google Cloud Translation. | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 4.16 (`translate_file`) |
| **DRAFT-01** | WebSocket `/drafts/{namespace}/{projectName}` real-time draft persistence with replay on reconnect. | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (Web UI component); [`docs/OPERATIONS.md`](docs/OPERATIONS.md) |

## v2 Requirements (deferred)

- **TRANS-02**: Google Translation V3 glossaries for consistent agent-optimized
  terminology. (Not implemented.)

## Notes on scope

What the MCP interface intentionally does *not* cover (history rewriting, push
subscriptions, arbitrary-commit diffing, ACLs) is documented in
[`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 8. Optional Bearer-token
authentication was added post-v1 (remediation directive) and is specified in
[`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) § 3.

## Traceability

| Req ID | Phase | Status |
|--------|-------|--------|
| META-01 | Phase 1 | Validated |
| META-02 | Phase 1 | Validated |
| FILE-01 | Phase 1 | Validated |
| FILE-02 | Phase 2 | Validated |
| VER-01 | Phase 2 | Validated |
| VER-02 | Phase 2 | Validated |
| TRANS-01 | Phase 3 | Validated |
| DRAFT-01 | Phase 4 | Validated |

---
*Historical requirements record. Architecture and operations content moved to
`docs/` and `README.md` on 2026-05-28 (documentation-consolidation directive);
this file now links to those documents instead of duplicating them.*
