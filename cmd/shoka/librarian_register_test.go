package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
)

// listToolNames builds the MCP server via setupMCPServer for the given config and
// returns the set of registered tool names, over a real in-memory tools/list.
func listToolNames(t *testing.T, cfg *config.Config) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s, err := storage.NewFSGitStorage(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	nc := notify.NewCenter(100)

	srv := setupMCPServer(ctx, cfg, s, logger, nc)

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
	}
	return names
}

// TestAskTheLibrarian_ConditionalRegistration: the tool is ABSENT when the LLM is
// not configured and PRESENT when it is (the translate_file/OAuth conditional
// pattern, B-73 Stage 5). The on-config uses the local-ollama placeholder, so no
// real key or network is involved — registration builds the client but never
// calls it.
func TestAskTheLibrarian_ConditionalRegistration(t *testing.T) {
	// Off: no llm block.
	off := &config.Config{}
	if listToolNames(t, off)["ask_the_librarian"] {
		t.Error("ask_the_librarian must be ABSENT when the LLM is not configured")
	}

	// On: a configured (ollama placeholder) LLM.
	on := &config.Config{}
	on.LLM = config.LLMConfig{
		Provider: "anthropic",
		BaseURL:  "http://localhost:11434",
		Model:    "Qwen3:1.7b-q4_K_M",
		MaxSteps: 4,
	}
	names := listToolNames(t, on)
	if !names["ask_the_librarian"] {
		t.Error("ask_the_librarian must be PRESENT when the LLM is configured")
	}
	// Core tools remain registered either way.
	if !names["read_file"] || !names["search_files"] {
		t.Error("core tools should always be present")
	}
}
