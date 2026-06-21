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

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// systemPrompt instructs the internal librarian LLM. The constraints it
// describes are advisory framing only — actual enforcement is in the Go guard
// that wraps every tool dispatch (B-49), not in this text.
const systemPrompt = "You are a librarian answering questions about a corpus of files. " +
	"Use the 'list' tool to discover files and the 'read' tool to read them. " +
	"Base your answer only on what you read. Be concise and answer the question directly. " +
	"All access is read-only and confined to the corpus; some paths may be refused."

// Request is one librarian query: a natural-language question over a corpus
// rooted at Root, with Root's contents filtered by IgnorePatterns (".git/" is
// always added by the guard).
type Request struct {
	Question       string
	Root           string
	IgnorePatterns []string
}

// Result is the librarian's answer plus the trace of tool calls it made (the
// paths it read/listed and any refusals) — useful for callers and for the
// structural assertions in tests.
type Result struct {
	Answer string
	Calls  []ToolCall
}

// Librarian runs ask_the_librarian queries against an injected LLM client.
type Librarian struct {
	client   llm.Client
	maxSteps int
}

// New builds a Librarian over an LLM client. maxSteps caps the tool-call loop's
// model round-trips (<= 0 falls back to a sensible default).
func New(client llm.Client, maxSteps int) *Librarian {
	return &Librarian{client: client, maxSteps: maxSteps}
}

// Ask answers a question over the request's corpus root. It builds a fresh
// guard + tool set for the root, runs the tool-call loop, and returns the
// answer and the call trace. The corpus bytes live only in the loop's
// conversation, which is discarded when Ask returns — that nesting is the
// feature (the caller's context never sees the corpus).
func (l *Librarian) Ask(ctx context.Context, req Request) (Result, error) {
	guard := NewGuard(req.Root, req.IgnorePatterns)
	tools := buildTools(guard)
	answer, calls, err := runLoop(ctx, l.client, systemPrompt, req.Question, tools, l.maxSteps)
	return Result{Answer: answer, Calls: calls}, err
}
