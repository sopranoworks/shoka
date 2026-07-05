package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/librariansrc"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
	"github.com/sopranoworks/shoka/pkg/librarian"
)

// librarianIgnore is the visible-surface ignore set the librarian is given for a
// Shoka project, on top of the guard's always-on ".git/". It hides Shoka's own
// dotfiles (e.g. ".shoka.disposable") so the librarian's view matches Shoka's
// intent. (The guard enforces root-confinement + symlink-skip regardless.)
var librarianIgnore = []string{".shoka*"}

type AskTheLibrarianInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project to consult"`
	Question    string `json:"question" jsonschema:"required, the natural-language question to answer from the project's documents"`
	ScopeHint   string `json:"scope_hint,omitempty" jsonschema:"optional, a sub-path or topic to focus the search on"`
}

type AskTheLibrarianSource struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet,omitempty"`
}

type AskTheLibrarianOutput struct {
	// Answer is the librarian's synthesized answer. The corpus itself is never
	// returned — the internal LLM reads it and only the answer comes back, so the
	// calling agent's context is not filled (B-73).
	Answer  string                  `json:"answer"`
	Sources []AskTheLibrarianSource `json:"sources,omitempty"`
}

// AskTheLibrarianHandler answers a natural-language question over a Shoka project
// using an internal, constraint-bearing LLM (B-73). The internal librarian reads
// the project through read-only, root-confined, ignore-filtered, symlink-skipping
// tools (enforced in Go, not the prompt); only the answer is returned. The outer
// call rides AuthzMiddleware, so the caller still needs the namespace/project
// scope; the inner tools are in-process Go, not separately MCP-exposed.
func AskTheLibrarianHandler(s *storage.FSGitStorage, lib *librarian.Librarian) func(context.Context, *mcp.CallToolRequest, AskTheLibrarianInput) (*mcp.CallToolResult, AskTheLibrarianOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input AskTheLibrarianInput) (*mcp.CallToolResult, AskTheLibrarianOutput, error) {
		errResult := func(msg string) (*mcp.CallToolResult, AskTheLibrarianOutput, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
				IsError: true,
			}, AskTheLibrarianOutput{}, nil
		}

		if input.ProjectName == "" || strings.TrimSpace(input.Question) == "" {
			return errResult("project_name and question are required")
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return errResult("invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed")
		}

		root, err := s.ProjectPath(input.Namespace, input.ProjectName)
		if err != nil {
			return errResult(fmt.Sprintf("invalid project: %v", err))
		}

		question := input.Question
		if h := strings.TrimSpace(input.ScopeHint); h != "" {
			question = fmt.Sprintf("Focus on: %s\n\n%s", h, question)
		}

		corpus := librariansrc.NewCorpus(s, input.Namespace, input.ProjectName)
		if lib.Classifier() != nil {
			corpus.WithVectorSearch(s)
		}
		res, err := lib.Ask(ctx, librarian.Request{
			Question:       question,
			Root:           root,
			IgnorePatterns: librarianIgnore,
			Corpus:         corpus,
		})
		if err != nil {
			return errResult(fmt.Sprintf("librarian failed: %v", err))
		}

		// Surface the files the librarian actually read as sources (read-only trace).
		var sources []AskTheLibrarianSource
		seen := map[string]bool{}
		for _, c := range res.Calls {
			if c.Tool == "read" && !c.Refused && c.Path != "" && !seen[c.Path] {
				seen[c.Path] = true
				sources = append(sources, AskTheLibrarianSource{Path: c.Path})
			}
		}
		return nil, AskTheLibrarianOutput{Answer: res.Answer, Sources: sources}, nil
	}
}
