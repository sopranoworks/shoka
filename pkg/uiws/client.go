// Package uiws is the reusable /ws/ui transport + protocol + auth/user/OAuth core
// of Shoka's WebSocket management surface (the 2026-06-21 GitYard core-extraction,
// Shape B). It holds the ws Client transport, the MessageType protocol + the
// table-parameterized authorization Gate, and the CoreHandlers slice (ACCOUNT_*/
// ADMIN_*/OAUTH_*/DOMAIN_*/CLIENT_*) with its three store interfaces — everything a
// second program (GitYard, a feature-reduced Shoka with no document store) needs to
// mount the auth/user/OAuth ws ops on its own manager. It depends ONLY on pkg/auth,
// pkg/authz, pkg/userstore, pkg/oauthstore and the stdlib/gorilla transport — never
// on internal/storage, internal/ui's document Manager, or internal/identity, so it
// can live under pkg/ and be imported across repos.
package uiws

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/authz"
)

// Client wraps one WebSocket connection with a write mutex. gorilla/websocket
// permits only one concurrent writer per connection; the read-loop's responses
// and the notify-drain goroutine both write, so every write goes through here.
//
// ID is this connection's sender identity (the 2026-06-01 sender-exclusion
// directive): the connection subscribes to the notify center under it, and its
// own writes carry it (via notify.WithSender on the write context) so the center
// does not echo the write back to this connection. It is unique per connection
// for the life of the process ("ws-<seq>").
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	// ID is this connection's sender identity ("ws-<seq>"), assigned by the manager.
	ID string
	// req is the upgraded connection's HTTP request, retained so the admin-gated
	// OAUTH_ISSUE_SELF handler can derive the RFC 8707 resource (forwarded-header
	// aware) the same way /authorize does. It is read-only after the upgrade.
	req *http.Request
	// principal is the authenticated WebUI session principal carried on the upgrade
	// request context (the B-28 stage-1 login: authapi.Middleware attaches it). Zero
	// when no user has logged in yet (the no-lockout single-operator path); when set,
	// hasPrincipal is true and the user's email becomes the git Author on web writes.
	principal    auth.Principal
	hasPrincipal bool
}

// NewClient builds a Client over an upgraded connection. id is the manager-assigned
// sender id ("ws-<seq>"). It captures the WebUI session principal (B-28 stage 1) from
// the upgrade request context — attached by authapi.Middleware from the session cookie
// — so web writes can be authored as the logged-in user and the gate can read the
// connection's scope. Absent when no user has logged in (the no-lockout path).
func NewClient(conn *websocket.Conn, id string, r *http.Request) *Client {
	c := &Client{conn: conn, ID: id, req: r}
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		c.principal = p
		c.hasPrincipal = true
	}
	return c
}

// Principal returns the connection's authenticated session principal (zero value when
// the connection carries none — the no-lockout single-operator path).
func (c *Client) Principal() auth.Principal { return c.principal }

// HasPrincipal reports whether the connection carries an authenticated session
// principal (a logged-in user).
func (c *Client) HasPrincipal() bool { return c.hasPrincipal }

// Scope returns the connection's authorization scope: the session principal's scope,
// or "*" (super-user) for the no-principal / no-lockout single-operator connection.
func (c *Client) Scope() string {
	if c.hasPrincipal {
		return c.principal.Scope
	}
	return "*"
}

// CanRead reports whether the connection may read the given namespace (the
// global-read result filter, B-28 stage 3).
func (c *Client) CanRead(namespace string) bool {
	return authz.Authorize(c.Scope(), namespace, "", authz.LevelRead) == nil
}

// WriteMessage marshals and sends one {type,payload} frame under the write mutex.
func (c *Client) WriteMessage(msgType MessageType, payload interface{}) error {
	payloadData, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	msg := WSMessage{Type: msgType, Payload: json.RawMessage(payloadData)}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// SendError sends a generic ERROR frame carrying a human-readable message.
func (c *Client) SendError(errMsg string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	msg := WSMessage{
		Type:    Error,
		Payload: json.RawMessage(fmt.Sprintf(`{"message": %q}`, errMsg)),
	}
	data, _ := json.Marshal(msg)
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

// SendResponse sends a typed response frame; on marshal failure it falls back to an
// ERROR frame.
func (c *Client) SendResponse(msgType MessageType, payload interface{}) {
	if err := c.WriteMessage(msgType, payload); err != nil {
		c.SendError("Failed to marshal response")
	}
}
