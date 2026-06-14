package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
)

// ---------------------------------------------------------------------------
// Unit tests: pattern parsing + matching + the no-broadcast / glob constraints.
// ---------------------------------------------------------------------------

func TestParsePattern_ShapeAndConstraints(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		target  string
		pred    string
		isGlob  bool
	}{
		{name: "ns/project only = whole project", raw: "ns/proj", target: "ns/proj", pred: ""},
		{name: "trailing slash = whole project", raw: "ns/proj/", target: "ns/proj", pred: ""},
		{name: "prefix", raw: "ns/proj/directives/", target: "ns/proj", pred: "directives/"},
		{name: "single-segment glob", raw: "ns/proj/directives/2026-*", target: "ns/proj", pred: "directives/2026-*", isGlob: true},
		{name: "missing project", raw: "ns", wantErr: true},
		{name: "empty namespace", raw: "/proj/x", wantErr: true},
		{name: "empty project", raw: "ns//x", wantErr: true},
		{name: "wildcard namespace rejected (no broadcast)", raw: "*/proj/x", wantErr: true},
		{name: "wildcard project rejected (no broadcast)", raw: "ns/*/x", wantErr: true},
		{name: "recursive ** rejected (would add a dep)", raw: "ns/proj/src/**", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parsePattern(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePattern(%q) = no error, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePattern(%q) error: %v", tc.raw, err)
			}
			if p.target != tc.target || p.pathPred != tc.pred || p.isGlob != tc.isGlob {
				t.Fatalf("parsePattern(%q) = {target:%q pred:%q glob:%v}, want {target:%q pred:%q glob:%v}",
					tc.raw, p.target, p.pathPred, p.isGlob, tc.target, tc.pred, tc.isGlob)
			}
		})
	}
}

func TestPatternMatches(t *testing.T) {
	prefix, _ := parsePattern("ns/proj/directives/")
	glob, _ := parsePattern("ns/proj/directives/2026-*")
	whole, _ := parsePattern("ns/proj")

	cases := []struct {
		p              pattern
		target, evPath string
		want           bool
	}{
		{prefix, "ns/proj", "directives/a.md", true},
		{prefix, "ns/proj", "directives/sub/a.md", true}, // prefix spans separators
		{prefix, "ns/proj", "specs/a.md", false},         // out of prefix
		{prefix, "other/proj", "directives/a.md", false}, // different project
		{glob, "ns/proj", "directives/2026-06.md", true},
		{glob, "ns/proj", "directives/2025-01.md", false},  // glob miss
		{glob, "ns/proj", "directives/sub/2026.md", false}, // single-segment: * does not cross /
		{whole, "ns/proj", "anything/at/all.md", true},     // whole project
	}
	for _, c := range cases {
		if got := c.p.matches(c.target, c.evPath); got != c.want {
			t.Errorf("%q.matches(%q,%q) = %v, want %v", c.p.raw, c.target, c.evPath, got, c.want)
		}
	}
}

func TestIsNotifiableKind_SweepDrop(t *testing.T) {
	deliver := []string{"file.write", "file.move", "file.delete"}
	drop := []string{"project.create", "catalog.invariant_violation", "lostfound.moved", "lostfound.disposed", ""}
	for _, k := range deliver {
		if !isNotifiableKind(k) {
			t.Errorf("kind %q should be notifiable", k)
		}
	}
	for _, k := range drop {
		if isNotifiableKind(k) {
			t.Errorf("kind %q should be dropped", k)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end harness: two REAL MCP clients over a loopback Streamable-HTTP
// server, asserting the five acceptance criteria (the B-45b directive §2.1).
// No mocked transport, no synthetic timing — non-delivery is proven with a
// sentinel barrier (file.write is published synchronously inside Write, so once
// a write_file call returns the event is already enqueued for matching sessions).
// ---------------------------------------------------------------------------

const notifyWait = 3 * time.Second

// notifTestServer is a fully-wired loopback server: real storage + Center, the
// write_file / create_project tools (so clients make real changes) and the
// subscribe / unsubscribe tools under test.
type notifTestServer struct {
	url    string
	center *notify.Center
	store  *storage.FSGitStorage
	mgr    *SubscriptionManager
	server *mcp.Server
}

func newNotifTestServer(t *testing.T) *notifTestServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-notif-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	center := notify.NewCenter(1000)
	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{NotifyCenter: center})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-test", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "write_file", Description: "write"}, WriteFileHandler(s))
	mgr := NewSubscriptionManager(center, s, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "subscribe", Description: "subscribe"}, mgr.SubscribeHandler())
	mcp.AddTool(srv, &mcp.Tool{Name: "unsubscribe", Description: "unsubscribe"}, mgr.UnsubscribeHandler())
	mgr.SetServer(srv)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ht := httptest.NewServer(handler)
	t.Cleanup(ht.Close)

	return &notifTestServer{url: ht.URL, center: center, store: s, mgr: mgr, server: srv}
}

// notifClient is a real MCP client that records the shoka.notify messages it
// receives on its standalone stream.
type notifClient struct {
	t    *testing.T
	cs   *mcp.ClientSession
	msgs chan notificationPayload
}

func (ts *notifTestServer) connect(t *testing.T, name string) *notifClient {
	t.Helper()
	nc := &notifClient{t: t, msgs: make(chan notificationPayload, 64)}
	c := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0.0.0"}, &mcp.ClientOptions{
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			if req.Params == nil || req.Params.Logger != notifyLogger {
				return
			}
			var p notificationPayload
			b, err := json.Marshal(req.Params.Data)
			if err != nil {
				return
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return
			}
			select {
			case nc.msgs <- p:
			default:
			}
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.url}, nil)
	if err != nil {
		t.Fatalf("%s connect: %v", name, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	nc.cs = cs
	return nc
}

func (nc *notifClient) setLevel() {
	nc.t.Helper()
	if err := nc.cs.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{Level: "debug"}); err != nil {
		nc.t.Fatalf("setLevel: %v", err)
	}
}

func (nc *notifClient) call(name string, args map[string]any) {
	nc.t.Helper()
	res, err := nc.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		nc.t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		nc.t.Fatalf("call %s returned tool error: %v", name, res.Content)
	}
}

// callExpectError calls a tool and requires a tool-level error result.
func (nc *notifClient) callExpectError(name string, args map[string]any) {
	nc.t.Helper()
	res, err := nc.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		nc.t.Fatalf("call %s: transport error: %v", name, err)
	}
	if !res.IsError {
		nc.t.Fatalf("call %s: expected a tool error, got success", name)
	}
}

func (nc *notifClient) subscribe(pattern string) {
	nc.call("subscribe", map[string]any{"pattern": pattern})
}

func (nc *notifClient) writeFile(project, path string) {
	nc.call("write_file", map[string]any{"project_name": project, "namespace": "ns", "path": path, "content": "x\n"})
}

// waitFor returns the next recorded notification, or fails if none arrives.
func (nc *notifClient) waitFor() notificationPayload {
	nc.t.Helper()
	select {
	case p := <-nc.msgs:
		return p
	case <-time.After(notifyWait):
		nc.t.Fatalf("timed out waiting for a notification")
		return notificationPayload{}
	}
}

// expectPath waits for a notification and asserts its path (the sentinel barrier:
// because matched events are delivered in publish order, the awaited sentinel
// proves any earlier non-matching/excluded event was already processed and not
// delivered).
func (nc *notifClient) expectPath(wantPath string) {
	nc.t.Helper()
	p := nc.waitFor()
	if p.Path != wantPath {
		nc.t.Fatalf("got notification for %q, want %q (an event that should not have been delivered slipped through)", p.Path, wantPath)
	}
}

func TestMCPNotifications_EndToEnd(t *testing.T) {
	ts := newNotifTestServer(t)

	// A is the subscriber under test; B is an external actor (its writes are not
	// A's, so they are delivered to A). Both hold their standalone stream open.
	a := ts.connect(t, "subscriber-A")
	b := ts.connect(t, "actor-B")
	a.setLevel()
	a.subscribe("ns/proj/watched/")

	// (a) In-scope external change IS delivered.
	b.writeFile("proj", "watched/a.md")
	got := a.waitFor()
	if got.Kind != "file.write" || got.Target != "ns/proj" || got.Path != "watched/a.md" {
		t.Fatalf("(a) scoped delivery: got %+v, want file.write ns/proj watched/a.md", got)
	}

	// (c) The subscriber's OWN write is NOT echoed (sender-exclusion). A writes,
	// then B writes an in-scope sentinel; A's next message must be the sentinel.
	a.writeFile("proj", "watched/self.md")
	b.writeFile("proj", "watched/sentinel-c.md")
	a.expectPath("watched/sentinel-c.md")

	// (b) A change OUTSIDE the subscribed scope is NOT delivered.
	b.writeFile("proj", "other/b.md")
	b.writeFile("proj", "watched/sentinel-b.md")
	a.expectPath("watched/sentinel-b.md")

	// (d) A sweep/internal event (lostfound.*, catalog.*) is NOT delivered, even
	// though its path matches — the kind filter drops it.
	ts.center.Notify("lostfound.moved", "ns/proj", "watched/swept.md")
	ts.center.Notify("catalog.invariant_violation", "ns/proj", "watched/bad.md")
	b.writeFile("proj", "watched/sentinel-d.md")
	a.expectPath("watched/sentinel-d.md")

	// (e) After unsubscribe, no further delivery. A witness W (still subscribed)
	// proves the post-unsubscribe write propagated; A must receive nothing.
	w := ts.connect(t, "witness-W")
	w.setLevel()
	w.subscribe("ns/proj/watched/")
	a.call("unsubscribe", map[string]any{}) // clear all
	b.writeFile("proj", "watched/after.md")
	w.expectPath("watched/after.md") // barrier: the event was processed
	select {
	case p := <-a.msgs:
		t.Fatalf("(e) post-unsubscribe silence: A received %+v, want nothing", p)
	default:
	}
}

// TestMCPNotifications_RejectsUnresolvedAndMalformed proves a pattern that does
// not resolve to a real project (or is malformed) is a subscribe-time error — no
// silent broadcast.
func TestMCPNotifications_RejectsUnresolvedAndMalformed(t *testing.T) {
	ts := newNotifTestServer(t)
	a := ts.connect(t, "subscriber-A")
	a.callExpectError("subscribe", map[string]any{"pattern": "ns/does-not-exist/x"})
	a.callExpectError("subscribe", map[string]any{"pattern": "*/*/x"})
	a.callExpectError("subscribe", map[string]any{"pattern": "ns/proj/src/**"})
	a.callExpectError("subscribe", map[string]any{"pattern": "bad"})
}

// TestMCPNotifications_ReapOnSessionClose proves a subscription is dropped when
// the session disconnects (the Server.Sessions() reaper — the directive's
// "after ... session close, no further delivery").
func TestMCPNotifications_ReapOnSessionClose(t *testing.T) {
	ts := newNotifTestServer(t)
	a := ts.connect(t, "subscriber-A")
	a.setLevel()
	a.subscribe("ns/proj/watched/")

	// The subscription exists.
	ts.mgr.mu.Lock()
	n := len(ts.mgr.sessions)
	ts.mgr.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 registered session, got %d", n)
	}

	// Close A's session, then reap. The reaper sees A is no longer live.
	_ = a.cs.Close()
	// Give the server a moment to observe the disconnect before reaping.
	deadline := time.Now().Add(notifyWait)
	for {
		ts.mgr.ReapOnce()
		ts.mgr.mu.Lock()
		n = len(ts.mgr.sessions)
		ts.mgr.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n != 0 {
		t.Fatalf("expected the disconnected session to be reaped, still have %d", n)
	}
}
