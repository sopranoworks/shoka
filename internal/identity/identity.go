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

type ctxKey struct{}

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
