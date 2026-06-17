// Package ingest is the single source of truth for byte-faithful file ingest:
// decoding a write's content per its content_encoding and enforcing the closed
// markdown/json/yaml format allowlist. It is shared by every server-side write
// front door — the MCP write_file tool (internal/tools) and the WebUI /ws/ui
// SAVE_FILE handler (internal/ui) — so the base64 + allowlist rule has ONE
// implementation, never a divergent copy (B-28 external file drag-and-drop add).
package ingest

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

// allowedExts is the CLOSED set of file extensions accepted on the base64 ingest
// path (content_encoding="base64"). It admits only the formats a coding agent can
// actually consume — markdown / json / yaml — and rejects everything else,
// including extensionless paths, so a binary "foreign object" an agent cannot use
// is refused at the boundary. Extension-based and case-insensitive; no content
// sniffing (predictable, no guessing). The restriction is scoped to the base64
// ingest path only — a plain (utf8) write is unaffected (operator-confirmed
// 2026-06-05, B-46c).
var allowedExts = map[string]bool{
	".md":       true,
	".markdown": true,
	".json":     true,
	".yaml":     true,
	".yml":      true,
}

// AllowedFormats is the human-facing list for error messages, in a stable order
// (kept in sync by hand with allowedExts — the set is tiny and closed).
const AllowedFormats = ".md, .markdown, .json, .yaml, .yml"

// IsAllowedFormat reports whether path carries an extension in the closed ingest
// allowlist (case-insensitive). An extensionless path returns false.
func IsAllowedFormat(path string) bool {
	return allowedExts[strings.ToLower(filepath.Ext(path))]
}

// DecodeContent resolves a write's content to the raw bytes to store, honouring
// encoding. For the base64 ingest path it first enforces the format allowlist (so
// the restriction binds every client server-side), then decodes. It returns the
// bytes to write, or a user-facing message + structured reason when the input is
// rejected (caller turns these into an error result). ok is false on rejection.
//
// Reason values on rejection: "format_rejected" (base64 path, extension outside
// the allowlist) and "invalid_encoding" (an unknown content_encoding, or
// malformed base64 content).
func DecodeContent(path, content, encoding string) (out, msg, reason string, ok bool) {
	switch encoding {
	case "", "utf8":
		// Literal text — the default behaviour, unchanged.
		return content, "", "", true
	case "base64":
		if !IsAllowedFormat(path) {
			return "", fmt.Sprintf("unsupported file format for ingest: %q; allowed formats are %s", path, AllowedFormats), "format_rejected", false
		}
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return "", fmt.Sprintf("invalid base64 content: %v", err), "invalid_encoding", false
		}
		return string(decoded), "", "", true
	default:
		return "", fmt.Sprintf("unsupported content_encoding %q; use \"utf8\" or \"base64\"", encoding), "invalid_encoding", false
	}
}
