package tests

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

var librarianE2ERan atomic.Bool

// TestLibrarianE2E_MCP exercises the complete MCP round-trip for
// ask_the_librarian: real HTTP transport, real tool handler, real
// librariansrc.Corpus over FSGitStorage, real LLM calls to LM Studio.
//
// This test uses an in-process MCP server (httptest) with the production
// AskTheLibrarianHandler wired in — the same handler the real Shoka server
// uses. The librariansrc.Corpus adapter, NOT dirCorpus, backs every search
// and read. The only difference from a full server is config loading /
// process lifecycle, which the existing live_http_* suite already covers.
//
// Skip: LM Studio not reachable, already ran in this process.
func TestLibrarianE2E_MCP(t *testing.T) {
	if !librarianE2ERan.CompareAndSwap(false, true) {
		t.Skip("librarian E2E already exercised once in this process")
	}

	baseURL := "http://localhost:1234/v1"
	if v := os.Getenv("LIBRARIAN_LMSTUDIO_BASE_URL"); v != "" {
		baseURL = v
	}
	model := "qwen3-1.7b"
	if v := os.Getenv("LIBRARIAN_LMSTUDIO_MODEL"); v != "" {
		model = v
	}

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.LastIndex(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL, err)
	}
	_ = conn.Close()

	// --- Storage + corpus ---
	s, err := storage.NewFSGitStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSGitStorage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.CreateProject("default", "e2e-lib"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "target.md",
		"# Build Tool\n\nfuigo installation was added to README.md on July 3, 2026.\n"+
			"The setup procedure is now documented in the project README.\n", ""); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "noise1.md",
		"# Overview\n\nThis project uses Go for the backend.\n", ""); err != nil {
		t.Fatalf("write noise1: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "noise2.md",
		"# Architecture\n\nThe storage layer uses filesystem isolation.\n", ""); err != nil {
		t.Fatalf("write noise2: %v", err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain timeout")
	}

	// --- LLM + librarian ---
	t.Setenv("OPENAI_API_KEY", "lm-studio")
	client, err := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    model,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := librarian.New(client, 0)

	// --- In-process MCP server with production handler ---
	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-librarian-e2e", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ask_the_librarian"}, tools.AskTheLibrarianHandler(s, lib))

	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	httpSrv := httptest.NewServer(h)
	defer httpSrv.Close()

	// --- MCP client ---
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	mcpCli := mcp.NewClient(&mcp.Implementation{Name: "librarian-e2e-client", Version: "0.0.0"}, nil)
	sess, err := mcpCli.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("MCP Connect: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// --- Call ask_the_librarian via MCP ---
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "ask_the_librarian",
		Arguments: map[string]any{
			"namespace":    "default",
			"project_name": "e2e-lib",
			"question":     "When was the build tool added to the documentation?",
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "connect:") {
			t.Skipf("LM Studio went away: %v", err)
		}
		t.Fatalf("CallTool ask_the_librarian: %v", err)
	}

	text := wireText(res)
	t.Logf("MCP response (IsError=%v): %s", res.IsError, text)
	t.Logf("GATE_RAW_LEAK=%v", strings.Contains(text, "<|"))

	if res.IsError {
		if strings.Contains(text, "connection refused") || strings.Contains(text, "connect:") {
			t.Skipf("LM Studio went away mid-call: %s", text)
		}
		t.Fatalf("E2E FAIL: ask_the_librarian returned error: %s", text)
	}

	// Parse structured output — the handler returns JSON with answer + sources.
	var output struct {
		Answer  string `json:"answer"`
		Sources []struct {
			Path string `json:"path"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Logf("Response is not JSON (raw: %q); checking as plain text", text)
		if strings.TrimSpace(text) == "" {
			t.Errorf("E2E FAIL: empty response from MCP")
		}
		return
	}

	if strings.TrimSpace(output.Answer) == "" {
		t.Errorf("E2E FAIL: empty answer in structured output")
	} else {
		t.Logf("Answer: %q", output.Answer)
	}

	if len(output.Sources) > 0 {
		for _, src := range output.Sources {
			t.Logf("Source: %s", src.Path)
		}
	}
}
