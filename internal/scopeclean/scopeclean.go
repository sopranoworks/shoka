// Package scopeclean is the cascade-cleanup adapter for the B-28 namespace/project
// management ops: when a namespace or project is deleted, every authorization grant that
// referenced it BY NAME must be removed from every persisted scope, so re-creating the
// same name never resurrects old access (leave-graceful is rejected). It spans the
// userstore (user accounts + pending invites) and the optional oauthstore (issued token
// series), pruning each scope with the authz grammar helpers.
//
// The Cleaner satisfies storage.ScopeCleaner structurally (it is NOT imported by storage —
// storage holds only the interface), so the go-git storage layer stays decoupled from the
// auth stores while still driving the cascade from inside DeleteProject/DeleteNamespace.
package scopeclean

import (
	"github.com/sopranoworks/shoka/internal/authz"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// scopeRewriter is the narrow capability a store exposes for cascade cleanup: apply a
// pure scope→scope transform to every persisted scope it holds, returning the count
// changed. Both *userstore.Store and *oauthstore.Store satisfy it.
type scopeRewriter interface {
	RewriteScopes(fn func(scope string) string) (int, error)
}

// Cleaner removes namespace/project grant references across the auth stores.
type Cleaner struct {
	stores []scopeRewriter
}

// New builds a Cleaner over the user store (required) and the OAuth store (optional —
// pass nil when OAuth is disabled). A typed-nil store is skipped, so cleanup spans only
// the stores that exist.
func New(users *userstore.Store, oauth *oauthstore.Store) *Cleaner {
	c := &Cleaner{}
	if users != nil {
		c.stores = append(c.stores, users)
	}
	if oauth != nil {
		c.stores = append(c.stores, oauth)
	}
	return c
}

// PurgeNamespace removes every grant referencing namespace ns (the namespace-wide grant
// and any project under it) from every persisted scope across the stores.
func (c *Cleaner) PurgeNamespace(ns string) error {
	return c.rewrite(func(scope string) string {
		out, _ := authz.PruneNamespaceGrants(scope, ns)
		return out
	})
}

// PurgeProject removes every grant referencing the specific project ns/proj from every
// persisted scope; namespace-wide and wildcard grants are left intact.
func (c *Cleaner) PurgeProject(ns, proj string) error {
	return c.rewrite(func(scope string) string {
		out, _ := authz.PruneProjectGrants(scope, ns, proj)
		return out
	})
}

// RewriteProject re-homes every grant referencing the project oldNs/proj to newNs/proj
// (namespace:<oldNs>/<proj>[:perm] → namespace:<newNs>/<proj>[:perm]) across every persisted
// scope — the project-move mirror of PurgeProject. Namespace-wide and wildcard grants are
// left intact (a move re-homes only the project-specific grant).
func (c *Cleaner) RewriteProject(oldNs, proj, newNs string) error {
	return c.rewrite(func(scope string) string {
		out, _ := authz.RewriteProjectGrants(scope, oldNs, proj, newNs)
		return out
	})
}

func (c *Cleaner) rewrite(fn func(scope string) string) error {
	if c == nil {
		return nil
	}
	for _, st := range c.stores {
		if _, err := st.RewriteScopes(fn); err != nil {
			return err
		}
	}
	return nil
}
