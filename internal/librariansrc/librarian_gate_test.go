package librariansrc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

var gateProductionRan atomic.Bool

const (
	gateBaseURL       = "http://localhost:1234/v1"
	gateLLMModel      = "qwen3-1.7b"
	gateEmbedModel    = "text-embedding-nomic-embed-text-v1.5"
	gateTargetPath    = "changelog/2026-07-03-fuigo-install.md"
	gateTargetContent = "# Build Tool Installation\n\n" +
		"fuigo installation was added to README.md on July 3, 2026.\n\n" +
		"The setup procedure for the build tool is now documented in the\n" +
		"project's main README file, including prerequisites, installation\n" +
		"steps, and configuration options.\n"
	gateQuestion = "When was the build tool's setup procedure added to the project documentation?"
)

var gateControlTokenRe = regexp.MustCompile(`<\|[^|]*\|>`)

// TestLibrarianGate_ProductionPath is the production-path gate test for the
// librarian. Unlike the dirCorpus-based gate test in pkg/librarian/, this test
// exercises the actual production code path:
//
//   - librariansrc.Corpus backed by FSGitStorage (not dirCorpus)
//   - Vector index via LM Studio embedding (when available)
//   - Chunk-level similarity filtering via the embedder
//   - Auto-read in search results
//   - Full tool-call loop with force-final-answer
//
// The corpus is 100+ benchmark files (MMLU categories) plus one known target.
// Assertions: non-empty answer, no control tokens, target file in sources.
//
// Skip: LM Studio not reachable, already ran in this process.
func TestLibrarianGate_ProductionPath(t *testing.T) {
	if !gateProductionRan.CompareAndSwap(false, true) {
		t.Skip("production path gate already exercised once in this process")
	}

	baseURL := gateEnvOr("LIBRARIAN_LMSTUDIO_BASE_URL", gateBaseURL)
	llmModel := gateEnvOr("LIBRARIAN_LMSTUDIO_MODEL", gateLLMModel)
	embedModel := gateEnvOr("LIBRARIAN_EMBED_MODEL", gateEmbedModel)

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.LastIndex(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL, err)
	}
	_ = conn.Close()

	// --- Storage + project ---
	s := newStore(t)
	project(t, s, "gate", "benchmark")

	fileCount := writeGateCorpus(t, s)
	write(t, s, "gate", "benchmark", gateTargetPath, gateTargetContent)
	fileCount++
	t.Logf("Wrote %d files to storage", fileCount)

	if !s.WaitForWAL(60 * time.Second) {
		t.Fatal("WAL drain timeout")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// --- Vector index (best-effort) ---
	vectorActive := false
	var embedder llm.Embedder
	t.Setenv("OPENAI_API_KEY", "lm-studio")

	embedder, embedErr := llm.NewEmbedder(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    embedModel,
	})
	if embedErr == nil {
		_, probeErr := embedder.Embed(ctx, "test")
		if probeErr == nil {
			s.SetVectorConfig(&storage.VectorIndexConfig{
				Embedder: embedder,
				Model:    embedModel,
			})
			s.StartVectorWorker(ctx, 0)
			vectorActive = gateWaitEmbeddings(t, s, fileCount, 120*time.Second)
			t.Logf("Vector index: active=%v", vectorActive)
		} else {
			t.Logf("Embedding probe failed (%v); proceeding without vector index", probeErr)
			embedder = nil
		}
	} else {
		t.Logf("Embedder creation failed (%v); proceeding without vector index", embedErr)
	}

	// --- Production corpus (mirrors AskTheLibrarianHandler exactly) ---
	corpus := NewCorpus(s, "gate", "benchmark").
		WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if vectorActive {
		corpus.WithVectorSearch(s)
	}
	if embedder != nil {
		corpus.WithChunkFilter(gateEmbedAdapter{embedder}, gateQuestion)
	}

	// --- LLM client ---
	client, clientErr := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    llmModel,
	})
	if clientErr != nil {
		t.Fatalf("NewClient: %v", clientErr)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := librarian.New(client, 0).WithLogger(logger)

	// --- Run ---
	root, rootErr := s.ProjectPath("gate", "benchmark")
	if rootErr != nil {
		t.Fatalf("ProjectPath: %v", rootErr)
	}

	res, askErr := lib.Ask(ctx, librarian.Request{
		Question:       gateQuestion,
		Root:           root,
		IgnorePatterns: []string{".shoka*"},
		Corpus:         corpus,
	})
	if askErr != nil {
		t.Logf("Debug log:\n%s", logBuf.String())
		if strings.Contains(askErr.Error(), "connection refused") || strings.Contains(askErr.Error(), "connect:") {
			t.Skipf("LM Studio went away: %v", askErr)
		}
		t.Fatalf("Ask failed: %v", askErr)
	}

	// --- Results ---
	answerPreview := res.Answer
	if len(answerPreview) > 300 {
		answerPreview = answerPreview[:300] + "…"
	}
	t.Logf("Answer: %q", answerPreview)
	t.Logf("Calls (%d):", len(res.Calls))
	for i, c := range res.Calls {
		t.Logf("  [%d] %s path=%q refused=%v", i, c.Tool, c.Path, c.Refused)
	}

	// --- Gate assertions ---
	if strings.TrimSpace(res.Answer) == "" {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
	}

	if gateControlTokenRe.MatchString(res.Answer) {
		t.Errorf("GATE FAIL: control tokens in answer: %q", answerPreview)
	}

	foundTarget := false
	for _, c := range res.Calls {
		if c.Tool == "read" && !c.Refused && strings.Contains(c.Path, "fuigo") {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: target file %q not in read sources", gateTargetPath)
	}
}

// --- Helpers ---

func gateEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// gateEmbedAdapter adapts llm.Embedder to ChunkEmbedder.
type gateEmbedAdapter struct{ e llm.Embedder }

func (a gateEmbedAdapter) EmbedText(ctx context.Context, text string) ([]float64, error) {
	vec, err := a.e.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	return vec.Values, nil
}

// gateWaitEmbeddings waits for at least target embeddings to complete, or
// returns false on timeout (non-fatal — fulltext search still works).
func gateWaitEmbeddings(t *testing.T, s *storage.FSGitStorage, target int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		_, _, embedded, failedEmbed, _, _ := s.VectorCounters()
		if int(embedded) >= target {
			t.Logf("All %d files embedded", embedded)
			return true
		}
		if int(embedded+failedEmbed) >= target {
			t.Logf("Embedding complete: %d embedded, %d failed", embedded, failedEmbed)
			return embedded > 0
		}
		select {
		case <-deadline:
			t.Logf("Embedding timeout: %d/%d embedded, %d failed", embedded, target, failedEmbed)
			return embedded > 0
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// --- MMLU corpus download ---
//
// Downloads actual MMLU benchmark questions (MIT license) from HuggingFace
// at test time. 6 categories × 20 questions = 120 files — enough to exercise
// the production search path without the embedding time of 800+ files.
// The 820-file scale test remains in pkg/librarian/librarian_gate_test.go.

var gateCategories = [...]string{
	"abstract_algebra", "anatomy", "astronomy",
	"computer_security", "machine_learning", "international_law",
}

type gateMMLURow struct {
	Question string   `json:"question"`
	Subject  string   `json:"subject"`
	Choices  []string `json:"choices"`
	Answer   int      `json:"answer"`
}

type gateMMLUResponse struct {
	Rows []struct {
		Row gateMMLURow `json:"row"`
	} `json:"rows"`
}

func writeGateCorpus(t *testing.T, s *storage.FSGitStorage) int {
	t.Helper()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	count := 0

	for _, cat := range gateCategories {
		rows, err := gateDownloadCategory(httpClient, cat)
		if err != nil {
			t.Skipf("MMLU download failed for %s: %v", cat, err)
		}
		for i, row := range rows {
			path := fmt.Sprintf("benchmark/%s/q%03d.md", cat, i+1)
			content := gateMMluDoc(cat, i+1, row)
			write(t, s, "gate", "benchmark", path, content)
			count++
		}
	}
	return count
}

func gateDownloadCategory(client *http.Client, category string) ([]gateMMLURow, error) {
	url := fmt.Sprintf(
		"https://datasets-server.huggingface.co/rows?dataset=cais/mmlu&config=%s&split=test&offset=0&length=20",
		category,
	)
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", category, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", category, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", category, err)
	}

	var data gateMMLUResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", category, err)
	}

	rows := make([]gateMMLURow, 0, len(data.Rows))
	for _, r := range data.Rows {
		rows = append(rows, r.Row)
	}
	return rows, nil
}

var gateAnswerLetters = [...]string{"A", "B", "C", "D"}

func gateMMluDoc(category string, num int, row gateMMLURow) string {
	title := strings.ReplaceAll(category, "_", " ")

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Question %d\n\n", title, num)
	fmt.Fprintf(&b, "%s\n\n", row.Question)
	for i, choice := range row.Choices {
		if i < 4 {
			fmt.Fprintf(&b, "- %s) %s\n", gateAnswerLetters[i], choice)
		}
	}
	letter := "A"
	if row.Answer >= 0 && row.Answer < 4 {
		letter = gateAnswerLetters[row.Answer]
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n", letter)
	return b.String()
}
