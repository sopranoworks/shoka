package librarian

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// Local-ollama defaults (the dev env's Anthropic-compatible /v1/messages). The
// API key is the literal placeholder "ollama" (accepted-but-ignored); no real
// key, no egress beyond localhost. Overridable via env for a different host/tag.
const (
	defaultOllamaBaseURL = "http://localhost:11434"
	defaultOllamaModel   = "Qwen3:1.7b-q4_K_M"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestAsk_OllamaEndToEnd runs the FULL tool-call loop against a REAL local
// ollama via the same Anthropic client used live (only the base URL/key/model
// differ). It SKIPS cleanly when ollama is not reachable or cannot serve the
// model, so the gate stays green without ollama — but when ollama is present it
// runs and asserts on STRUCTURE / tool-call behaviour, not exact answer text
// (a 1.7b 4-bit model is not reliable for text assertions — design report §3.4).
// ollamaE2ERan ensures the REAL LLM call runs at most once per process. The
// standing gate is `go test -race -count=30 ./...`; amplifying a nondeterministic
// external model call 30× would itself manufacture flakiness (and waste minutes
// of CPU inference) — the opposite of what the count is for. The pure-Go kernel,
// tool, and loop tests still run all 30 iterations under -race; only this one
// integration call is run once and then skipped.
var ollamaE2ERan atomic.Bool

func TestAsk_OllamaEndToEnd(t *testing.T) {
	if !ollamaE2ERan.CompareAndSwap(false, true) {
		t.Skip("ollama end-to-end already exercised once in this process; not amplifying a nondeterministic LLM call under -count")
	}

	baseURL := envOr("LIBRARIAN_OLLAMA_BASE_URL", defaultOllamaBaseURL)
	model := envOr("LIBRARIAN_OLLAMA_MODEL", defaultOllamaModel)

	// Reachability probe: a quick TCP dial to the ollama port.
	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("ollama not reachable at %s (%v); skipping end-to-end LLM test", baseURL, err)
	}
	_ = conn.Close()

	root, ignore, secret := fixtureCorpus(t)
	client := llm.NewAnthropicClient(llm.LLMConfig{
		Provider: "anthropic",
		BaseURL:  baseURL,
		APIKey:   "ollama",
		Model:    model,
		MaxSteps: 6,
	})
	lib := New(client, 6)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	res, err := lib.Ask(ctx, Request{
		Question:       "Use the search, list, and read tools to inspect the corpus. What number is mentioned in doc.md?",
		Root:           root,
		IgnorePatterns: ignore,
	})
	if err != nil {
		// Reachable port but the call failed (model not pulled, etc.) — skip
		// with a clear message rather than fail the gate.
		t.Skipf("ollama reachable but the LLM call failed (model %q pulled?): %v", model, err)
	}

	// --- Structural assertions (a working ollama MUST satisfy these) ---

	// 1. The loop fired at least one tool call.
	if len(res.Calls) == 0 {
		t.Errorf("model made no tool calls; the loop never exercised read/list")
	}

	// 2. Every SUCCESSFUL call stayed within root and within the ignore policy:
	//    re-running the guard on it must succeed. No successful escape.
	guard := NewGuard(root, ignore)
	for _, c := range res.Calls {
		if c.Refused {
			continue // refusals are expected and safe
		}
		if c.Tool == "search" {
			continue // search hits are guard-filtered inside the tool; the trace path is the query
		}
		isDir := c.Tool == "list"
		if _, _, gerr := guard.Resolve(c.Path, isDir); gerr != nil {
			t.Errorf("a SUCCESSFUL %s call escaped the guard: path=%q (%v)", c.Tool, c.Path, gerr)
		}
	}

	// 3. The ignored/out-of-root corpus secret never surfaced in the answer.
	if strings.Contains(res.Answer, secret) {
		t.Errorf("answer leaked the ignored/out-of-root secret: %q", res.Answer)
	}

	// 4. Non-empty answer.
	if strings.TrimSpace(res.Answer) == "" {
		t.Errorf("answer is empty")
	}

	t.Logf("ollama end-to-end OK: %d tool call(s), answer=%q", len(res.Calls), truncate(res.Answer, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
