// Package authz is the single authorization decision shared by every Shoka access
// surface (the B-28 stage-2 enforcement flip). Both the MCP tools/call middleware
// (internal/tools) and the WebUI /ws/ui dispatch gate (internal/ui) call the one
// Authorize here — the same decision function, two callers — so there is exactly one
// choke point even though the surfaces obtain their principal separately (the MCP
// token vs the WebUI session, kept apart by the B-50 decoupling).
//
// Scope is the principal's grant list — the existing comma-separated scope string
// also carried on auth.Principal.Scope, the OAuth token (oauthstore.SeriesRecord.Scope),
// and the WebUI user (userstore.UserRecord.Scope). Each grant is a target with a
// permission level:
//
//   - super-user (admin over every namespace)
//     *:admin | *:rw | *:r    wildcard at that level
//     namespace:<ns>:<perm>            that namespace, any project, at <perm>
//     namespace:<ns>/<proj>:<perm>     that project only, at <perm>
//     namespace:<ns>          legacy, level-less: read-write (preserves "all actions in
//     the namespace" without granting admin)
//
// <perm> is admin | rw | r. A bare "*" and an EMPTY scope ("") both parse to a single
// super-user grant — the backward-compatible ZERO VALUE: a JSON-decoded legacy record
// (Scope == "") is super-user, all-access, exactly as before the level dimension
// existed, so no migration runs.
//
// Levels order read ⊂ write ⊂ admin. A principal's EFFECTIVE level at a target is the
// MAXIMUM level over every grant that matches the target (most-permissive-wins — the
// safe fallback if duplicate/overlapping grants for one target ever occur; the
// management UI intends one grant per namespace). A check passes iff the effective
// level ≥ the operation's required level. A GLOBAL operation (no target namespace,
// e.g. list_projects) is satisfied by the principal's maximum level anywhere — so a
// super-user always passes and any positive-level principal may perform a global read.
package authz

import (
	"fmt"
	"strings"
)

// Level is an ordered permission level. Higher is more permissive.
type Level int

const (
	// LevelNone is the absence of any access (a principal with no matching grant).
	LevelNone Level = iota
	// LevelRead permits read operations.
	LevelRead
	// LevelWrite permits read + write operations.
	LevelWrite
	// LevelAdmin permits read + write + administrative operations.
	LevelAdmin
)

// String returns the wire/log token for a level.
func (l Level) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelAdmin:
		return "admin"
	default:
		return "none"
	}
}

// Grant is one parsed scope entry: a target (wildcard, or a namespace optionally
// narrowed to a project) at a permission level.
type Grant struct {
	Zone      string // "" for Shoka-native scopes; non-empty for downstream zones (e.g. "git")
	Wildcard  bool   // target is "*" — matches every namespace
	Namespace string
	Project   string // "" = namespace-wide
	Level     Level
}

// ParseScope parses a comma-separated scope string into grants. A bare "*" or an
// empty string yields a single super-user grant (the zero value = today's all-access).
// An unparseable/level-less namespace grant maps to read-write (legacy compatibility).
func ParseScope(scope string) []Grant {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "*" {
		return []Grant{{Wildcard: true, Level: LevelAdmin}}
	}
	var grants []Grant
	for _, raw := range strings.Split(scope, ",") {
		g := strings.TrimSpace(raw)
		if g == "" {
			continue
		}
		grants = append(grants, parseGrant(g))
	}
	if len(grants) == 0 {
		return []Grant{{Wildcard: true, Level: LevelAdmin}}
	}
	return grants
}

func parseGrant(g string) Grant {
	var zone string
	if i := strings.Index(g, "/"); i >= 0 {
		colonPos := strings.Index(g, ":")
		if colonPos < 0 || i < colonPos {
			zone = g[:i]
			g = g[i+1:]
		}
	}

	var grant Grant
	switch {
	case g == "*":
		grant = Grant{Wildcard: true, Level: LevelAdmin}
	case g == NoAccessScope:
		grant = Grant{Level: LevelNone}
	default:
		if rest, lvl, ok := splitPerm(g); ok && rest == "*" {
			grant = Grant{Wildcard: true, Level: lvl}
		} else if strings.HasPrefix(g, "namespace:") {
			target := strings.TrimPrefix(g, "namespace:")
			rest, lvl, ok := splitPerm(target)
			if !ok {
				rest, lvl = target, LevelWrite
			}
			ns, proj, _ := strings.Cut(rest, "/")
			grant = Grant{Namespace: ns, Project: proj, Level: lvl}
		} else if rest, lvl, ok := splitPerm(g); ok {
			ns, proj, _ := strings.Cut(rest, ":")
			if ns != "" {
				grant = Grant{Namespace: ns, Project: proj, Level: lvl}
			} else {
				grant = Grant{Level: LevelNone}
			}
		} else {
			grant = Grant{Level: LevelNone}
		}
	}
	grant.Zone = zone
	return grant
}

// splitPerm strips a trailing ":admin" / ":rw" / ":r" suffix, returning the target,
// its level, and whether a suffix was present.
func splitPerm(s string) (rest string, lvl Level, ok bool) {
	switch {
	case strings.HasSuffix(s, ":admin"):
		return strings.TrimSuffix(s, ":admin"), LevelAdmin, true
	case strings.HasSuffix(s, ":rw"):
		return strings.TrimSuffix(s, ":rw"), LevelWrite, true
	case strings.HasSuffix(s, ":r"):
		return strings.TrimSuffix(s, ":r"), LevelRead, true
	default:
		return s, LevelNone, false
	}
}

// EffectiveLevel is the principal's maximum permission level at (namespace, project)
// across all matching grants. For a global operation (namespace == "") every grant
// contributes, so the result is the principal's maximum level anywhere.
func EffectiveLevel(grants []Grant, namespace, project string) Level {
	var max Level
	for _, g := range grants {
		if matches(g, namespace, project) && g.Level > max {
			max = g.Level
		}
	}
	return max
}

func matches(g Grant, namespace, project string) bool {
	if g.Zone != "" {
		return false // Shoka ignores zoned scopes; downstream consumers filter by zone
	}
	if namespace == "" {
		return true // global op: every grant contributes to the max
	}
	if g.Wildcard {
		return true
	}
	if g.Namespace != namespace {
		return false
	}
	if g.Project == "" {
		return true // namespace-wide grant
	}
	return g.Project == project
}

// Authorize reports whether scope grants at least `required` at the target
// (namespace, project). It returns nil on allow and a descriptive error on deny.
func Authorize(scope, namespace, project string, required Level) error {
	if EffectiveLevel(ParseScope(scope), namespace, project) >= required {
		return nil
	}
	target := namespace
	if target == "" {
		target = "(global)"
	}
	return fmt.Errorf("scope does not permit %s access to %s", required, target)
}
