// Package librarian is the reusable, constraint-bearing LLM query module
// behind the ask_the_librarian MCP tool (backlog B-73). An internal "librarian"
// LLM reads a large corpus through a read-only, root-confined, ignore-filtered,
// symlink-skipping tool set and returns only the ANSWER, so the calling agent's
// context is never filled with the corpus.
//
// This is the Stages 0-2 vertical slice (per the design report): the LLM-client
// seam (pkg/librarian/llm), the constraint kernel (guard.go + ignore.go), and a
// read+list tool set + tool-call loop proven end-to-end against local ollama.
// There is deliberately NO search tool, NO Shoka index/data-source adapter, NO
// MCP registration, and NO result cache yet — those are later stages.
//
// pkg/librarian imports no internal/storage, internal/config, internal/ui, and
// no go-git (the dependency-free IgnoreMatcher keeps the archlint boundary
// green).
package librarian

import (
	"context"
	"log/slog"
	"sync"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// systemPrompt instructs the internal librarian LLM. The constraints it
// describes are advisory framing only — actual enforcement is in the Go guard
// that wraps every tool dispatch (B-49), not in this text.
const systemPrompt = "You are a librarian answering questions about a corpus of files. " +
	"Use the 'search' tool to find relevant files by content, the 'list' tool to " +
	"discover files, and the 'read' tool to read them. " +
	"The search tool automatically reads the top results and includes their content " +
	"in the response — use that content to answer directly. Use 'read' for files not " +
	"in the top results or to read a specific section via offset/limit. " +
	"Your answer must be based on file content, not on search result titles alone. " +
	"Be concise and answer the question directly. When multiple " +
	"documents are relevant, list all of them with a brief description of each — do not " +
	"narrow to a single answer when several candidates exist. Include all relevant source " +
	"paths in your response. All access is read-only and confined to the corpus; some " +
	"paths may be refused."

// Request is one librarian query: a natural-language question over a corpus
// rooted at Root, with Root's contents filtered by IgnorePatterns (".git/" is
// always added by the guard). Corpus is the data source; when nil it defaults
// to a filesystem corpus rooted at Root (the fixture/debug path). Products
// inject their own Corpus (e.g. the Shoka index-backed adapter).
type Request struct {
	Question       string
	Root           string
	IgnorePatterns []string
	Corpus         Corpus
}

// Result is the librarian's answer plus the trace of tool calls it made (the
// paths it read/listed and any refusals) — useful for callers and for the
// structural assertions in tests.
type Result struct {
	Answer    string // post-strip (control tokens removed)
	RawAnswer string // pre-strip (as the model produced it)
	Calls     []ToolCall
}

// Librarian runs ask_the_librarian queries against an injected LLM client.
//
// The client is a SWAPPABLE reference, guarded by mu, so an operator-triggered
// config reload can replace it live (a new model/provider) without a restart.
// The model is captured inside the client at construction (the seam carries no
// per-call model), so a swap means installing a NEW client, not mutating a field
// — see SetClient. Ask snapshots the reference under the lock and releases it
// before the tool-call loop, so no lock ever spans an LLM round-trip and an
// in-flight Ask completes on the client it started with.
type Librarian struct {
	mu         sync.RWMutex
	client     llm.Client
	maxSteps   int
	classifier Classifier // nil when not configured
	log        *slog.Logger
}

// New builds a Librarian over an LLM client. maxSteps caps the tool-call loop's
// model round-trips (<= 0 falls back to a sensible default).
func New(client llm.Client, maxSteps int) *Librarian {
	return &Librarian{client: client, maxSteps: maxSteps}
}

// WithLogger attaches a structured logger for debug diagnostics of the
// tool-call loop. A nil logger (default) discards output.
func (l *Librarian) WithLogger(log *slog.Logger) *Librarian {
	l.log = log
	return l
}

// WithClassifier returns the Librarian with the given classifier attached.
// Pass nil to leave the classifier unconfigured.
func (l *Librarian) WithClassifier(c Classifier) *Librarian {
	l.classifier = c
	return l
}

// Classifier returns the attached classifier, or nil if not configured.
func (l *Librarian) Classifier() Classifier {
	return l.classifier
}

// SetClient atomically swaps the live LLM client (e.g. after a config reload to a
// new model/provider). In-flight Ask calls already snapshotted the previous
// client and finish on it; the next Ask picks up the new one.
func (l *Librarian) SetClient(client llm.Client) {
	l.mu.Lock()
	l.client = client
	l.mu.Unlock()
}

// SetMaxSteps atomically swaps the tool-call loop budget. In-flight Ask calls
// already snapshotted the previous value; the next Ask picks up the new one.
func (l *Librarian) SetMaxSteps(n int) {
	l.mu.Lock()
	l.maxSteps = n
	l.mu.Unlock()
}

// MaxSteps returns the current tool-call loop budget.
func (l *Librarian) MaxSteps() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.maxSteps
}

// currentClient returns the live client reference under the read lock. The caller
// uses the returned value without holding the lock, so a concurrent SetClient can
// proceed and no lock spans the subsequent round-trips.
func (l *Librarian) currentClient() llm.Client {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.client
}

func (l *Librarian) logger() *slog.Logger {
	if l.log != nil {
		return l.log
	}
	return slog.New(discardHandler{})
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler            { return d }

// Ask answers a question over the request's corpus root. It builds a fresh
// guard + tool set for the root, runs the tool-call loop, and returns the
// answer and the call trace. The corpus bytes live only in the loop's
// conversation, which is discarded when Ask returns — that nesting is the
// feature (the caller's context never sees the corpus).
func (l *Librarian) Ask(ctx context.Context, req Request) (Result, error) {
	guard := NewGuard(req.Root, req.IgnorePatterns)
	corpus := req.Corpus
	if corpus == nil {
		corpus = NewFSCorpus(req.Root)
	}
	tools := buildTools(guard, corpus)
	// Snapshot the swappable references before the loop so a concurrent
	// SetClient/SetMaxSteps never affects this in-flight call.
	answer, calls, err := runLoop(ctx, l.currentClient(), systemPrompt, req.Question, tools, l.MaxSteps(), l.logger())
	return Result{Answer: answer, RawAnswer: answer, Calls: calls}, err
}
