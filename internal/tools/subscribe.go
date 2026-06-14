package tools

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/notify"
)

// This file implements B-45b: scoped MCP change notifications. An MCP client
// subscribes to fully-addressed file-change patterns; Shoka delivers only the
// matching, non-self-originated, external file events to that session over the
// standard MCP logging notification channel.
//
// Design (the 2026-06-05 B-45b directive, operator-confirmed):
//   - Model: Redis pub/sub. Each session holds a FLAT SET of patterns; subscribe
//     adds, unsubscribe removes one or clears all, and an event is delivered if it
//     matches ANY pattern in the set (OR-match).
//   - Pattern: "<namespace>/<project>/<path-predicate>". namespace and project are
//     REQUIRED and literal (no wildcards) — there is no "*/*" all-projects back
//     door — and must resolve to an existing project at subscribe time. The path
//     part is a prefix (the floor) or a single-segment glob via stdlib path.Match;
//     recursive "**" is not supported (it would add a dependency) and is rejected.
//   - Filter: only file.write / file.move / file.delete are notification-worthy.
//     project.create, catalog.*, and lostfound.* (sweep/internal kinds) are
//     dropped — background-sweep observability is metrics' job (B-41), not this.
//   - Delivery: ServerSession.Log (notifications/message) carrying
//     {kind,target,path,source_path}. It is per-session and the client opts in via
//     logging/setLevel. Log performs a network write, so it runs on a per-session
//     DRAIN goroutine fed by a bounded channel that drops on full — never on the
//     notify.Center dispatch path (the B-44a no-slow-work-on-dispatch discipline).
//   - Sender-exclusion: each session subscribes to the Center as
//     SubscribeAs("mcp:"+sessionID), reusing the built-in exclusion so a session
//     never receives its own writes (MCP writes are already tagged "mcp:"+id).
//   - Lifetime: session-scoped, reaped against Server.Sessions() (the SDK exposes
//     no tool-session close hook, so Shoka reaps — the B-21 precedent).

// reaperInterval is how often the background reaper drops subscriptions whose MCP
// session is no longer live. Low-churn, single-user: a coarse interval is fine.
const reaperInterval = 30 * time.Second

// notifyChanBuffer bounds each session's delivery channel. A full channel drops
// (lossy-but-safe): a slow or dead subscriber loses events rather than stalling
// the Center publisher. Mirrors internal/ui/manager.go's 64-deep buffer.
const notifyChanBuffer = 64

// notifyLogger is the logger name carried on the notifications/message so a client
// can distinguish Shoka change notifications from any other server log output.
const notifyLogger = "shoka.notify"

// projectResolver is the slice of storage the manager needs to validate that a
// pattern's namespace/project resolves to a real project at subscribe time.
// *storage.FSGitStorage satisfies it.
type projectResolver interface {
	ListProjects(namespace string) ([]string, error)
}

// notificationPayload is the structured Data carried on the notifications/message.
// It is pure content addressing (namespace/project/path) plus the change kind —
// never a token, secret, or deployment address (the directive's confidentiality
// constraint). source_path is set only for file.move (the old path).
type notificationPayload struct {
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Path       string `json:"path"`
	SourcePath string `json:"source_path,omitempty"`
}

// pattern is a parsed subscription pattern. target is the joined
// "<namespace>/<project>" (matched exactly against an event's Target); pathPred is
// the path part, matched as a prefix (isGlob false) or a single-segment glob
// (isGlob true, via path.Match).
type pattern struct {
	raw      string
	target   string
	pathPred string
	isGlob   bool
}

// matches reports whether event e falls within this pattern: same project, and a
// path that satisfies the prefix or single-segment glob.
func (p pattern) matches(target, evPath string) bool {
	if target != p.target {
		return false
	}
	if p.isGlob {
		ok, err := path.Match(p.pathPred, evPath)
		return err == nil && ok
	}
	return strings.HasPrefix(evPath, p.pathPred)
}

// containsGlobMeta reports whether s contains a path.Match metacharacter.
func containsGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// parsePattern parses "<namespace>/<project>/<path-predicate>" into a pattern.
// namespace and project are required and literal; the path predicate is optional
// (absent/empty = the whole project). It rejects wildcard namespace/project (the
// no-broadcast constraint) and the unsupported recursive "**" glob.
func parsePattern(raw string) (pattern, error) {
	parts := strings.SplitN(raw, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return pattern{}, fmt.Errorf("pattern must be '<namespace>/<project>[/<path>]' with a non-empty namespace and project")
	}
	ns, proj := parts[0], parts[1]
	if containsGlobMeta(ns) || containsGlobMeta(proj) {
		return pattern{}, fmt.Errorf("namespace and project must be literal (no wildcards): there is no all-projects subscription")
	}
	pathPred := ""
	if len(parts) == 3 {
		pathPred = parts[2]
	}
	if strings.Contains(pathPred, "**") {
		return pattern{}, fmt.Errorf("recursive '**' glob is not supported; use a path prefix (e.g. 'src/') or a single-segment glob (e.g. 'src/*.md')")
	}
	return pattern{
		raw:      raw,
		target:   ns + "/" + proj,
		pathPred: pathPred,
		isGlob:   containsGlobMeta(pathPred),
	}, nil
}

// patternNamespaceProject splits a parsed pattern's target back into namespace and
// project for resolution.
func (p pattern) namespaceProject() (string, string) {
	ns, proj, _ := strings.Cut(p.target, "/")
	return ns, proj
}

// isNotifiableKind reports whether an event kind is an external file change worth
// delivering. It is an allowlist: write/move/delete only. Everything else
// (project.create, catalog.*, lostfound.*) is a sweep/internal kind and dropped.
func isNotifiableKind(kind string) bool {
	switch kind {
	case "file.write", "file.move", "file.delete":
		return true
	default:
		return false
	}
}

// sessionSub holds one MCP session's subscription state: its pattern set, the
// Center unsubscribe, and the bounded channel + drain goroutine that perform the
// (slow) Log delivery off the Center dispatch path.
type sessionSub struct {
	ss    *mcp.ServerSession
	unsub func()
	ch    chan notify.Event
	done  chan struct{}
	once  sync.Once // guards teardown (reaper vs unsubscribe race)

	mu       sync.RWMutex
	patterns map[string]pattern
}

// onEvent is the notify.Center callback. It runs on the publisher's goroutine
// under the Center's subscriber read lock, so it MUST be fast and non-blocking: it
// filters by kind, OR-matches the session's patterns, and enqueues (dropping on a
// full buffer). The slow Log send happens on the drain goroutine, never here.
func (sub *sessionSub) onEvent(e notify.Event) {
	if !isNotifiableKind(e.Kind) {
		return
	}
	sub.mu.RLock()
	matched := false
	for _, p := range sub.patterns {
		if p.matches(e.Target, e.Path) {
			matched = true
			break
		}
	}
	sub.mu.RUnlock()
	if !matched {
		return
	}
	select {
	case sub.ch <- e:
	default:
		// Buffer full: drop. Lossy-but-safe — a slow/dead subscriber must never
		// stall the Center. Drop observability is a B-41 follow-up.
	}
}

// drain delivers queued events to the client via ServerSession.Log until the
// subscription is torn down. Log silently no-ops if the client has not issued
// logging/setLevel; a delivery error (e.g. a closing session) is ignored — the
// reaper will drop the subscription.
func (sub *sessionSub) drain(logger *slog.Logger) {
	for {
		select {
		case <-sub.done:
			return
		case e := <-sub.ch:
			err := sub.ss.Log(context.Background(), &mcp.LoggingMessageParams{
				Level:  "info",
				Logger: notifyLogger,
				Data: notificationPayload{
					Kind:       e.Kind,
					Target:     e.Target,
					Path:       e.Path,
					SourcePath: e.SourcePath,
				},
			})
			if err != nil && logger != nil {
				logger.Debug("notification delivery failed", "session", sub.ss.ID(), "error", err)
			}
		}
	}
}

// teardown stops Center delivery and the drain goroutine. It is safe to call more
// than once (reaper and unsubscribe may race) and MUST be called with no manager
// or session lock held: sub.unsub() acquires the Center's subscriber lock, and the
// Center fan-out path acquires that lock before sub.mu — calling unsub under sub.mu
// would invert the order and deadlock.
func (sub *sessionSub) teardown() {
	sub.once.Do(func() {
		if sub.unsub != nil {
			sub.unsub()
		}
		close(sub.done)
	})
}

// SubscriptionManager owns the per-session subscription registry, the Center
// consumers, and the reaper. One manager is constructed per server.
type SubscriptionManager struct {
	center   *notify.Center
	resolver projectResolver
	logger   *slog.Logger

	server atomic.Pointer[mcp.Server] // set post-construction, for reaping

	mu       sync.Mutex
	sessions map[string]*sessionSub
}

// NewSubscriptionManager creates a manager over the given Center, using resolver
// to validate a pattern's namespace/project at subscribe time.
func NewSubscriptionManager(center *notify.Center, resolver projectResolver, logger *slog.Logger) *SubscriptionManager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SubscriptionManager{
		center:   center,
		resolver: resolver,
		logger:   logger,
		sessions: make(map[string]*sessionSub),
	}
}

// SetServer wires the constructed *mcp.Server so the reaper can enumerate live
// sessions. Called once, after the server is built.
func (m *SubscriptionManager) SetServer(s *mcp.Server) { m.server.Store(s) }

// resolves reports whether a pattern's namespace/project names a real project. A
// pattern that does not resolve is a subscribe-time error, never a silent
// broadcast.
func (m *SubscriptionManager) resolves(p pattern) error {
	ns, proj := p.namespaceProject()
	projects, err := m.resolver.ListProjects(ns)
	if err != nil {
		return fmt.Errorf("namespace %q does not resolve: %w", ns, err)
	}
	for _, name := range projects {
		if name == proj {
			return nil
		}
	}
	return fmt.Errorf("project %q does not exist in namespace %q", proj, ns)
}

// addPattern registers p for the given session, creating the session's Center
// subscription + drain goroutine on first use, and returns the session's full
// pattern set after the add.
func (m *SubscriptionManager) addPattern(ss *mcp.ServerSession, p pattern) []string {
	m.mu.Lock()
	sub := m.sessions[ss.ID()]
	if sub == nil {
		sub = &sessionSub{
			ss:       ss,
			ch:       make(chan notify.Event, notifyChanBuffer),
			done:     make(chan struct{}),
			patterns: make(map[string]pattern),
		}
		// Each session subscribes under its own "mcp:"+id identity so the Center's
		// built-in sender-exclusion drops the session's own writes for free.
		sub.unsub = m.center.SubscribeAs("mcp:"+ss.ID(), sub.onEvent)
		m.sessions[ss.ID()] = sub
		go sub.drain(m.logger)
	}
	m.mu.Unlock()

	sub.mu.Lock()
	sub.patterns[p.raw] = p
	list := patternList(sub.patterns)
	sub.mu.Unlock()
	return list
}

// removePattern removes one pattern from a session's set, tearing the session down
// if its set becomes empty. Returns the remaining set and whether anything was
// removed.
func (m *SubscriptionManager) removePattern(sessionID, raw string) (remaining []string, removed bool) {
	m.mu.Lock()
	sub := m.sessions[sessionID]
	if sub == nil {
		m.mu.Unlock()
		return nil, false
	}
	sub.mu.Lock()
	_, removed = sub.patterns[raw]
	delete(sub.patterns, raw)
	empty := len(sub.patterns) == 0
	remaining = patternList(sub.patterns)
	sub.mu.Unlock()
	if empty {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if empty {
		sub.teardown() // no lock held — see teardown's contract
	}
	return remaining, removed
}

// clearSession removes all of a session's patterns and tears it down. Returns
// whether the session had any subscription.
func (m *SubscriptionManager) clearSession(sessionID string) bool {
	m.mu.Lock()
	sub := m.sessions[sessionID]
	if sub != nil {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if sub == nil {
		return false
	}
	sub.teardown()
	return true
}

// ReapOnce drops subscriptions whose MCP session is no longer live. It is exported
// so tests can reap deterministically; production also runs it on a ticker. If the
// server pointer is unset it does nothing (never reaps everything).
func (m *SubscriptionManager) ReapOnce() {
	srv := m.server.Load()
	if srv == nil {
		return
	}
	live := make(map[string]bool)
	for ss := range srv.Sessions() {
		live[ss.ID()] = true
	}

	m.mu.Lock()
	var dead []*sessionSub
	for id, sub := range m.sessions {
		if !live[id] {
			dead = append(dead, sub)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, sub := range dead {
		sub.teardown()
	}
}

// StartReaper runs ReapOnce on a ticker until ctx is cancelled.
func (m *SubscriptionManager) StartReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(reaperInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.ReapOnce()
			}
		}
	}()
}

// patternList returns the sorted raw patterns of a set (a stable, copyable view
// for tool output).
func patternList(set map[string]pattern) []string {
	out := make([]string, 0, len(set))
	for raw := range set {
		out = append(out, raw)
	}
	sort.Strings(out)
	return out
}

// SubscribeInput is the subscribe tool argument: one fully-addressed pattern added
// to this session's set (Redis SUBSCRIBE semantics).
type SubscribeInput struct {
	Pattern string `json:"pattern" jsonschema:"required, a change-notification pattern '<namespace>/<project>/<path>'. namespace and project are required and literal (no wildcards). The path part is a prefix (e.g. 'directives/') or a single-segment glob via path.Match (e.g. 'directives/2026-*'); recursive '**' is not supported. Adds the pattern to this session's subscription set; the session then receives a notifications/message for each external file.write/file.move/file.delete under a matching pattern (never its own writes)"`
}

// SubscribeOutput reports the registered pattern and the session's full set.
type SubscribeOutput struct {
	Subscribed bool     `json:"subscribed"`
	Pattern    string   `json:"pattern"`
	Patterns   []string `json:"patterns"`
}

// UnsubscribeInput is the unsubscribe tool argument: a pattern to remove, or empty
// to clear all of this session's subscriptions (Redis UNSUBSCRIBE semantics).
type UnsubscribeInput struct {
	Pattern string `json:"pattern,omitempty" jsonschema:"optional, the pattern to remove from this session's subscription set; omit to clear ALL of this session's subscriptions"`
}

// UnsubscribeOutput reports what happened and the remaining set.
type UnsubscribeOutput struct {
	Unsubscribed bool     `json:"unsubscribed"`
	Cleared      bool     `json:"cleared"`
	Patterns     []string `json:"patterns"`
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// SubscribeHandler returns the subscribe tool handler. It validates the pattern
// (shape + namespace/project resolution) then adds it to the calling session's set.
func (m *SubscriptionManager) SubscribeHandler() mcp.ToolHandlerFor[SubscribeInput, SubscribeOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in SubscribeInput) (*mcp.CallToolResult, SubscribeOutput, error) {
		if req == nil || req.Session == nil {
			return toolError("subscribe requires an MCP session"), SubscribeOutput{}, nil
		}
		p, err := parsePattern(in.Pattern)
		if err != nil {
			return toolError(err.Error()), SubscribeOutput{}, nil
		}
		if err := m.resolves(p); err != nil {
			return toolError(err.Error()), SubscribeOutput{}, nil
		}
		list := m.addPattern(req.Session, p)
		return nil, SubscribeOutput{Subscribed: true, Pattern: p.raw, Patterns: list}, nil
	}
}

// UnsubscribeHandler returns the unsubscribe tool handler. With a pattern it
// removes that one; with no pattern it clears all of the session's subscriptions.
func (m *SubscriptionManager) UnsubscribeHandler() mcp.ToolHandlerFor[UnsubscribeInput, UnsubscribeOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in UnsubscribeInput) (*mcp.CallToolResult, UnsubscribeOutput, error) {
		if req == nil || req.Session == nil {
			return toolError("unsubscribe requires an MCP session"), UnsubscribeOutput{}, nil
		}
		if in.Pattern == "" {
			cleared := m.clearSession(req.Session.ID())
			return nil, UnsubscribeOutput{Unsubscribed: cleared, Cleared: true}, nil
		}
		remaining, removed := m.removePattern(req.Session.ID(), in.Pattern)
		return nil, UnsubscribeOutput{Unsubscribed: removed, Patterns: remaining}, nil
	}
}
