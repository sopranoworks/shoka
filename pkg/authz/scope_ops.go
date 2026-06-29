package authz

import (
	"sort"
	"strings"
)

// NoAccessScope is a scope that grants nothing (LevelNone everywhere). It is the
// substitute the cascade-cleanup helpers return when pruning a deleted namespace/project
// would otherwise leave an EMPTY scope — and an empty scope ("") is the migration-free
// zero value that ParseScope reads as SUPER-USER (the documented footgun). A principal
// whose only grants referenced a deleted name must drop to no access, never silently
// escalate to all-access, so an emptied scope becomes this instead.
const NoAccessScope = "none"

// IsSuperUser reports whether scope carries a wildcard admin grant — the strict
// "super-user" predicate (admin over EVERY namespace), distinct from "admin on some
// namespace". The B-28 namespace create/delete ops authorize through THIS, never the
// loose Authorize(scope, "", "", LevelAdmin) global form: a global op matches every
// grant (matches() returns true when namespace==""), so a mere namespace:<ns>:admin
// would wrongly satisfy it. The migration-free zero value ("" or "*") is super-user.
func IsSuperUser(scope string) bool {
	for _, g := range ParseScope(scope) {
		if g.Zone == "" && g.Wildcard && g.Level >= LevelAdmin {
			return true
		}
	}
	return false
}

// AdminNamespaces returns the namespaces on which scope grants admin, and a bool that is
// true when the principal is a super-user (admin over EVERY namespace — the caller should
// then enumerate all namespaces rather than this set, and the returned slice is nil). For
// a scoped principal it is the sorted, de-duplicated set of namespaces carrying an
// admin-level namespace-wide grant (namespace:<ns>:admin). It is the server-side basis
// for part 2's admin-filtered namespace listing ("namespaces I administer").
func AdminNamespaces(scope string) (namespaces []string, superUser bool) {
	if IsSuperUser(scope) {
		return nil, true
	}
	seen := make(map[string]bool)
	for _, g := range ParseScope(scope) {
		if g.Zone == "" && !g.Wildcard && g.Namespace != "" && g.Project == "" && g.Level >= LevelAdmin {
			if !seen[g.Namespace] {
				seen[g.Namespace] = true
				namespaces = append(namespaces, g.Namespace)
			}
		}
	}
	sort.Strings(namespaces)
	return namespaces, false
}

// PruneNamespaceGrants removes every grant that references namespace ns BY NAME — the
// namespace-wide grant (namespace:<ns>[:perm]) and any project under it
// (namespace:<ns>/<proj>[:perm]) — and returns the rewritten scope plus the count
// removed. Wildcard (super-user) grants and grants for other namespaces are preserved
// VERBATIM. Used by the cascade cleanup when a namespace is deleted.
func PruneNamespaceGrants(scope, ns string) (newScope string, removed int) {
	return pruneScope(scope, func(g Grant) bool {
		return g.Zone == "" && !g.Wildcard && g.Namespace == ns
	})
}

// PruneProjectGrants removes only the grants that reference the specific project
// ns/proj by name (namespace:<ns>/<proj>[:perm]); the namespace-wide grant
// (namespace:<ns>) and wildcard grants are left intact. Returns the rewritten scope and
// the count removed. Used by the cascade cleanup when a single project is deleted.
func PruneProjectGrants(scope, ns, proj string) (newScope string, removed int) {
	return pruneScope(scope, func(g Grant) bool {
		return g.Zone == "" && !g.Wildcard && g.Namespace == ns && g.Project == proj
	})
}

// RewriteProjectGrants re-homes every grant that references the project oldNs/oldProj BY NAME
// — namespace:<oldNs>/<oldProj>[:perm] → namespace:<newNs>/<newProj>[:perm], the perm
// preserved — and returns the rewritten scope plus the count rewritten. ONE helper serves
// both project-level special ops (B-28): a MOVE passes oldProj==newProj (only the namespace
// changes); a project RENAME passes oldNs==newNs (only the project changes). Every OTHER grant
// (the namespace-wide namespace:<oldNs>, wildcards, other projects) is preserved VERBATIM,
// because re-homing a single project never moves namespace-wide authority. Surviving tokens
// keep their exact text (legacy level-less grants are not silently rewritten).
func RewriteProjectGrants(scope, oldNs, oldProj, newNs, newProj string) (newScope string, rewritten int) {
	var out []string
	for _, raw := range strings.Split(scope, ",") {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		if g := parseGrant(tok); g.Zone == "" && !g.Wildcard && g.Namespace == oldNs && g.Project == oldProj {
			out = append(out, "namespace:"+newNs+"/"+newProj+projectGrantSuffix(tok))
			rewritten++
			continue
		}
		out = append(out, tok)
	}
	if rewritten == 0 {
		return scope, 0
	}
	return strings.Join(out, ","), rewritten
}

// RewriteNamespaceGrants re-homes every grant that references namespace old BY NAME to new —
// BOTH the namespace-wide grant (namespace:<old>[:perm] → namespace:<new>[:perm]) AND every
// project-specific grant under it (namespace:<old>/<proj>[:perm] → namespace:<new>/<proj>[:perm])
// — the perm and the project segment preserved, and returns the rewritten scope plus the count
// rewritten. It is the namespace-rename mirror of PruneNamespaceGrants (which removes the same
// set): a namespace rename relabels the WHOLE namespace, so unlike a project move (which leaves
// namespace-wide grants alone) it MUST follow both forms. Wildcard (super-user) grants and
// grants for other namespaces are preserved VERBATIM; surviving tokens keep their exact text.
func RewriteNamespaceGrants(scope, old, new string) (newScope string, rewritten int) {
	var out []string
	for _, raw := range strings.Split(scope, ",") {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		if g := parseGrant(tok); g.Zone == "" && !g.Wildcard && g.Namespace == old {
			rebuilt := "namespace:" + new
			if g.Project != "" {
				rebuilt += "/" + g.Project
			}
			out = append(out, rebuilt+projectGrantSuffix(tok))
			rewritten++
			continue
		}
		out = append(out, tok)
	}
	if rewritten == 0 {
		return scope, 0
	}
	return strings.Join(out, ","), rewritten
}

// projectGrantSuffix returns the trailing permission suffix of a grant token (":admin" /
// ":rw" / ":r") or "" for a legacy level-less grant — so a rewrite preserves the exact
// perm form (including the legacy no-suffix form).
func projectGrantSuffix(tok string) string {
	for _, s := range []string{":admin", ":rw", ":r"} {
		if strings.HasSuffix(tok, s) {
			return s
		}
	}
	return ""
}

// pruneScope removes every comma-separated grant token for which drop(parsed) is true,
// preserving the SURVIVING tokens verbatim (no re-serialization, so a legacy level-less
// grant is not silently rewritten). It returns the new scope and the number removed. If
// nothing was removed the original scope is returned unchanged (so "" and "*" — which no
// by-name predicate matches — pass through untouched). If removal would leave an empty
// scope, NoAccessScope is returned instead (the empty-scope-is-super-user footgun).
func pruneScope(scope string, drop func(Grant) bool) (string, int) {
	var kept []string
	removed := 0
	for _, raw := range strings.Split(scope, ",") {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		if drop(parseGrant(tok)) {
			removed++
			continue
		}
		kept = append(kept, tok)
	}
	if removed == 0 {
		return scope, 0
	}
	if len(kept) == 0 {
		return NoAccessScope, removed
	}
	return strings.Join(kept, ","), removed
}
