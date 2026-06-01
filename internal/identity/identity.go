// Package identity resolves and formats the "who wrote this" recorded on every
// git commit Shoka produces: the owning user, the agent (MCP client) that
// performed the write, and an optional Rohrpost worker id.
//
// PROVISIONAL. This is single-user mode — the floor of a larger authentication
// design (maintenance backlog B-28), NOT that design. There is no authentication
// here: User is the one configured operator. The shape is chosen so a future
// authenticated multi-user mechanism substitutes a per-request user at the one
// resolution site (Resolve) without changing the commit format or the WAL, and
// so a suspended Rohrpost can populate WorkerID later with no format change.
package identity

import (
	"context"
	"fmt"
	"strings"
)

// CommitIdentity is the resolved identity for one commit.
type CommitIdentity struct {
	UserName  string // owning user (single-user mode; future: per-request user)
	UserEmail string
	AgentName string // the MCP client that wrote it, or the configured default
	WorkerID  string // Rohrpost worker id, or "" (the reserved, optional slot)

	// AuthorIsUser flips the git Author from the agent to the owning user. Set by
	// Resolve when the write carries a WithUser declaration (the web /ws/ui
	// SAVE_FILE path: a human acting as themselves, not an agent). The committer
	// and the Shoka-* trailers are unchanged; only which signature becomes the git
	// Author. Carried on the WAL entry so the async commit worker honours it.
	AuthorIsUser bool
}

// Defaults are the configured single-user identity plus the fallback agent
// identity used when an MCP client declares nothing. Held by storage, set from
// config at startup.
type Defaults struct {
	UserName    string
	UserEmail   string
	AgentName   string
	AgentWorker string
}

// Agent is a per-request agent self-declaration extracted from the MCP session
// (clientInfo.name + initialize _meta worker id) and carried on the context.
type Agent struct {
	Name   string
	Worker string
}

// User is a per-request owning-user declaration. It marks a write as performed by
// the human operator acting as themselves (the web /ws/ui SAVE_FILE path) rather
// than by an agent. In single-user mode the fields are left empty and Resolve
// falls back to the configured operator user; a future authentication layer
// (B-28) substitutes the authenticated user here at the same call site.
type User struct {
	Name  string
	Email string
}

type ctxKey struct{}
type userCtxKey struct{}

// WithAgent attaches a per-request agent declaration to ctx. The MCP tool
// handler sets this from the connecting client's initialize info; other write
// paths (web UI, translation) leave it unset and fall through to the default.
func WithAgent(ctx context.Context, a Agent) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// AgentFrom returns the agent declaration carried on ctx, if any.
func AgentFrom(ctx context.Context) (Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(Agent)
	return a, ok
}

// WithUser marks the write on ctx as authored by the owning user rather than an
// agent. Pass an empty User in single-user mode (Resolve fills the configured
// operator); pass a populated User once authentication can identify the actor.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userCtxKey{}, u)
}

// UserFrom returns the owning-user declaration carried on ctx, if any.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userCtxKey{}).(User)
	return u, ok
}

// Resolve combines the configured defaults with any per-request agent
// declaration on ctx into the identity for one commit.
//
// PROVISIONAL: User is always the configured single-user identity. A future
// authentication layer (B-28) substitutes a per-request authenticated user
// HERE — this is the single resolution site the directive's feasibility check 1
// relies on; no other code path treats single-user as a hardcoded invariant.
func Resolve(ctx context.Context, d Defaults) CommitIdentity {
	id := CommitIdentity{
		UserName:  d.UserName,
		UserEmail: d.UserEmail,
		AgentName: d.AgentName,
		WorkerID:  d.AgentWorker,
	}
	if a, ok := AgentFrom(ctx); ok {
		if a.Name != "" {
			id.AgentName = a.Name
		}
		if a.Worker != "" {
			id.WorkerID = a.Worker
		}
	}
	// A WithUser declaration makes the owning user the git Author (not the agent).
	// An empty User keeps the configured single-user; a populated one (future auth)
	// substitutes the authenticated actor at this single resolution site.
	if u, ok := UserFrom(ctx); ok {
		id.AuthorIsUser = true
		if u.Name != "" {
			id.UserName = u.Name
			id.UserEmail = u.Email
		}
	}
	return id
}

// WithDefaults fills any empty field from d. Used when reading an older WAL entry
// written before identity fields existed, so its commit still gets an intentional
// author rather than a zero one.
func (id CommitIdentity) WithDefaults(d Defaults) CommitIdentity {
	if id.UserName == "" {
		id.UserName = d.UserName
	}
	if id.UserEmail == "" {
		id.UserEmail = d.UserEmail
	}
	if id.AgentName == "" {
		id.AgentName = d.AgentName
	}
	// WorkerID is legitimately empty (no worker); do not default it.
	return id
}

// AgentEmail synthesises the email for the agent's native git author field, e.g.
// "claude-code" -> "claude-code@agents.shoka.local".
func AgentEmail(agentName string) string {
	local := sanitizeEmailLocal(agentName)
	if local == "" {
		local = "agent"
	}
	return local + "@agents.shoka.local"
}

func sanitizeEmailLocal(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// Trailers returns the canonical Shoka-* trailer block appended to the commit
// message — the unambiguous, parseable-backwards record of all three layers.
// Shoka-Worker is emitted only when present, so Rohrpost populates it later with
// no format change. The block ends with a trailing newline.
func (id CommitIdentity) Trailers() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Shoka-User: %s <%s>\n", id.UserName, id.UserEmail)
	fmt.Fprintf(&b, "Shoka-Agent: %s\n", id.AgentName)
	if id.WorkerID != "" {
		fmt.Fprintf(&b, "Shoka-Worker: %s\n", id.WorkerID)
	}
	return b.String()
}
