package librarian

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

const (
	defaultLMStudioBaseURL = "http://localhost:1234/v1"
	defaultLMStudioModel   = "qwen3-1.7b"
)

var lmStudioE2ERan atomic.Bool

// TestAsk_LMStudioEmptyAnswer is an integration test against LM Studio at
// localhost:1234 that reproduces the "empty answer with correct sources" bug.
// It captures full raw HTTP request/response for every LLM API call so the
// root cause can be identified from the wire traffic.
func TestAsk_LMStudioEmptyAnswer(t *testing.T) {
	if !lmStudioE2ERan.CompareAndSwap(false, true) {
		t.Skip("LM Studio integration already exercised once in this process")
	}

	baseURL := envOr("LIBRARIAN_LMSTUDIO_BASE_URL", defaultLMStudioBaseURL)
	model := envOr("LIBRARIAN_LMSTUDIO_MODEL", defaultLMStudioModel)

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.LastIndex(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("LM Studio not reachable at %s (%v); skipping", baseURL, err)
	}
	_ = conn.Close()

	// Build corpus with known content.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "file1.md"),
		"fuigo installation was added to README.md on July 3, 2026\n")
	writeFile(t, filepath.Join(root, "file2.md"),
		"The project uses Go for backend development\n")

	t.Setenv("OPENAI_API_KEY", "lm-studio")
	client := llm.NewCapturingClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    model,
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := New(client, 6).WithLogger(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	res, err := lib.Ask(ctx, Request{
		Question: "When was the build tool added to the documentation?",
		Root:     root,
	})
	if err != nil {
		dumpExchanges(t, client)
		if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "connect:") {
			t.Skipf("LM Studio went away mid-test: %v", err)
		}
		t.Fatalf("Ask failed: %v", err)
	}

	// Always dump raw HTTP traffic so we can inspect it.
	dumpExchanges(t, client)

	// Log the debug output.
	t.Logf("--- DEBUG LOG ---\n%s", logBuf.String())

	// Log the result.
	t.Logf("Answer: %q", res.Answer)
	t.Logf("Calls: %d", len(res.Calls))
	for i, c := range res.Calls {
		t.Logf("  call[%d]: tool=%s path=%s refused=%v detail=%s", i, c.Tool, c.Path, c.Refused, c.Detail)
	}

	// Check message format issues in the captured exchanges.
	checkMessageFormat(t, client)

	if strings.TrimSpace(res.Answer) == "" {
		t.Errorf("REPRODUCED: empty answer with %d tool calls", len(res.Calls))
		t.Logf("This confirms the empty-answer bug. See raw HTTP traffic above.")
	} else {
		t.Logf("Answer was non-empty — bug did not reproduce in this run")
	}
}

func dumpExchanges(t *testing.T, cc *llm.CapturingClient) {
	t.Helper()

	// Write exchanges to a file for easier inspection.
	outPath := filepath.Join(os.TempDir(), "librarian-lmstudio-traffic.log")
	var fileBuf bytes.Buffer

	for i, ex := range cc.Exchanges {
		header := formatExchangeHeader(i, ex)
		t.Logf("%s", header)
		fileBuf.WriteString(header + "\n")

		reqStr := prettyJSON(ex.RequestBody)
		t.Logf("REQUEST BODY:\n%s", truncateStr(reqStr, 3000))
		fileBuf.WriteString("REQUEST BODY:\n" + reqStr + "\n")

		respStr := prettyJSON(ex.ResponseBody)
		t.Logf("RESPONSE BODY:\n%s", truncateStr(respStr, 3000))
		fileBuf.WriteString("RESPONSE BODY:\n" + respStr + "\n\n")
	}

	if err := os.WriteFile(outPath, fileBuf.Bytes(), 0o644); err == nil {
		t.Logf("Full traffic log written to: %s", outPath)
	}
}

func formatExchangeHeader(i int, ex llm.CapturedExchange) string {
	return fmt.Sprintf("=== Exchange %d === %s %s → %d", i, ex.Method, ex.URL, ex.StatusCode)
}

func checkMessageFormat(t *testing.T, cc *llm.CapturingClient) {
	t.Helper()
	for i, ex := range cc.Exchanges {
		if len(ex.RequestBody) == 0 {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(ex.RequestBody, &body); err != nil {
			t.Logf("Exchange %d: request body is not valid JSON: %v", i, err)
			continue
		}

		msgs, ok := body["messages"].([]any)
		if !ok {
			continue
		}

		// Check role ordering: system → user → assistant → user (tool results) → ...
		var roles []string
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			roles = append(roles, role)
		}
		t.Logf("Exchange %d role sequence: %s", i, strings.Join(roles, " → "))

		// Check for empty content in any message.
		for j, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			content := mm["content"]
			if content == nil {
				t.Logf("Exchange %d msg[%d] (role=%s): content is null", i, j, mm["role"])
			}
			if s, ok := content.(string); ok && s == "" {
				t.Logf("Exchange %d msg[%d] (role=%s): content is empty string", i, j, mm["role"])
			}
		}

		// Check tool_calls in assistant messages.
		for j, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			if role == "assistant" {
				tc, hasTc := mm["tool_calls"]
				content := mm["content"]
				t.Logf("Exchange %d msg[%d] (assistant): has_tool_calls=%v content_type=%T content_preview=%s",
					i, j, hasTc, content, truncateStr(prettyJSON(marshalAny(content)), 200))
				if hasTc {
					t.Logf("Exchange %d msg[%d] tool_calls: %s", i, j,
						truncateStr(prettyJSON(marshalAny(tc)), 500))
				}
			}
			if role == "tool" {
				toolCallID, _ := mm["tool_call_id"].(string)
				content, _ := mm["content"].(string)
				t.Logf("Exchange %d msg[%d] (tool): tool_call_id=%s content_len=%d preview=%s",
					i, j, toolCallID, len(content), truncateStr(content, 200))
			}
		}

		// Check response for finish_reason and choices.
		if len(ex.ResponseBody) > 0 {
			var resp map[string]any
			if err := json.Unmarshal(ex.ResponseBody, &resp); err == nil {
				choices, _ := resp["choices"].([]any)
				for ci, ch := range choices {
					choice, _ := ch.(map[string]any)
					fr, _ := choice["finish_reason"].(string)
					msg, _ := choice["message"].(map[string]any)
					content := msg["content"]
					tc := msg["tool_calls"]
					t.Logf("Exchange %d response choice[%d]: finish_reason=%s content_type=%T has_tool_calls=%v",
						i, ci, fr, content, tc != nil)
					if content != nil {
						t.Logf("Exchange %d response content: %s", i, truncateStr(prettyJSON(marshalAny(content)), 500))
					}
					if tc != nil {
						t.Logf("Exchange %d response tool_calls: %s", i, truncateStr(prettyJSON(marshalAny(tc)), 500))
					}
				}
			}
		}
	}
}

func prettyJSON(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return string(b)
	}
	return out.String()
}

func marshalAny(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
