# v1 Requirements: Markdown-MCP Backend Server

## Overview
This document defines the scoped requirements for version 0.1 of Shoka. The goal is a functional prototype that handles multi-project isolated storage, version control, and a translation bridge for agents.

## v1 Requirements

### Metadata & Storage
- [x] **META-01**: User projects are physically isolated on the filesystem under a `<base_dir>/<namespace>/<projectName>` directory layout. Isolation is enforced by name validation (alphanumeric, `-`, `_`) plus path-traversal guards. No UUIDs are used.
- [x] **META-02**: Project identity is the human-readable `namespace/projectName` path itself; there is no separate metadata database. (The originally-planned SQLite UUID→name mapping was dropped in favour of direct filesystem isolation; this decision is now final.)

### File & Protocol
- [x] **FILE-01**: Expose project creation, file management, and translation triggers as MCP Tools.
- [x] **FILE-02**: Implement basic CRUD (Create, Read, Update, Delete) for Markdown files within a project directory.

### Versioning
- [x] **VER-01**: Every human "Send" action triggers an atomic Git commit for the modified file(s) using `go-git`.
- [x] **VER-02**: Expose Git commit history as MCP Resources or Tools to provide agents with contextual evolution.

### Translation
- [x] **TRANS-01**: Implement a manual, human-triggered translation pipeline (Japanese to English) using Google Cloud Translation API.

### Persistence
- [x] **DRAFT-01**: Implement a WebSocket-based `/drafts/{namespace}/{projectName}` endpoint for real-time draft persistence to prevent data loss on mobile/unstable clients. The draft file is replayed to the client on (re)connection so a session can resume from the last synced state.

## v2 Requirements (Deferred)
- **TRANS-02**: Support for Google Translation V3 glossaries to ensure consistent agent-optimized terminology.

## Out of Scope
- **Automatic Translation**: Translation remains an explicit human action.
- **Full Web UI**: Focusing on backend logic and MCP interface. (A functional React editor nevertheless ships and is embedded in the binary.)
- **Multi-user Authentication**: v0.1 assumes a trusted environment or single-operator service. (Optional Bearer-token authentication is introduced post-v1 by the 2026-05-27 remediation directive.)

## Traceability Matrix
| Req ID | Phase | Plan | Status |
|--------|-------|------|--------|
| META-01 | Phase 1 | | Validated |
| META-02 | Phase 1 | | Validated |
| FILE-01 | Phase 1 | | Validated |
| FILE-02 | Phase 2 | | Validated |
| VER-01 | Phase 2 | | Validated |
| VER-02 | Phase 2 | | Validated |
| TRANS-01 | Phase 3 | | Validated |
| DRAFT-01 | Phase 4 | | Validated |

---
*Last updated: 2026-05-27 — reconciled with the implemented system (remediation directive, Phase 1). TRANS-01 and DRAFT-01 confirmed complete; META-01/02 corrected to describe filesystem isolation (no UUIDs, no SQLite).*
