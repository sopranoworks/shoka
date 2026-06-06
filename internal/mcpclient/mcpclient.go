// Package mcpclient is the thin MCP client the Shoka CLI is built on (B-46b). It
// is deliberately minimal: connect to a Shoka MCP endpoint over Streamable HTTP
// with a Bearer access token, and call tools. It carries NO Shoka-specific
// judgement — no ingest rules, no format decisions, no catalog knowledge. All such
// logic lives in the server-side tools; this package only invokes them. That is
// the standing Shoka principle (server-side notification filtering, no base_dir
// direct-write, catalog/guard logic server-side) applied to the client.
//
// Authentication is a single Authorization: Bearer header, injected by a tiny
// RoundTripper on the SDK's StreamableClientTransport.HTTPClient. The richer
// browser+PKCE OAuth flow (StreamableClientTransport.OAuthHandler) is deferred —
// the foundation uses the token-to-self credential the operator pastes in.
package mcpclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerTransport injects "Authorization: Bearer <token>" on every request. It
// clones the request before mutating headers so it never rewrites a caller's
// shared *http.Request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r2)
}

// Session is a connected MCP client session. Close it when done.
type Session struct {
	cs *mcp.ClientSession
}

// Connect dials the MCP endpoint with the given Bearer token and completes the
// MCP initialize handshake. A nil/expired/rejected token surfaces as a connect
// error (the server answers the handshake POST with 401).
func Connect(ctx context.Context, endpoint, token string) (*Session, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mcpclient: empty endpoint")
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "shoka-cli", Version: "0.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearerTransport{token: token}},
	}
	cs, err := c.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: connect %s: %w", endpoint, err)
	}
	return &Session{cs: cs}, nil
}

// Close ends the session.
func (s *Session) Close() error {
	if s == nil || s.cs == nil {
		return nil
	}
	return s.cs.Close()
}

// CallTool invokes an MCP tool by name with the given arguments and returns the
// raw result. It is the only call surface the CLI needs — every subcommand is a
// thin wrapper that builds args, calls a tool, and renders the result.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("mcpclient: call %s: %w", name, err)
	}
	return res, nil
}
