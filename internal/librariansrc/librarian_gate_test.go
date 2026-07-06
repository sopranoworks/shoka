package librariansrc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
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

// --- Corpus generation ---
//
// MMLU-inspired categories. 6 categories × 20 files = 120 files, enough to
// exercise the production search path without the embedding time of 800+ files.
// The 820-file scale test remains in pkg/librarian/librarian_gate_test.go.

var gateCategories = [...]string{
	"abstract_algebra", "anatomy", "astronomy",
	"computer_security", "machine_learning", "international_law",
}

func writeGateCorpus(t *testing.T, s *storage.FSGitStorage) int {
	t.Helper()
	count := 0
	for _, cat := range gateCategories {
		for q := 1; q <= 20; q++ {
			path := fmt.Sprintf("benchmark/%s/q%03d.md", cat, q)
			content := gateDoc(cat, q)
			write(t, s, "gate", "benchmark", path, content)
			count++
		}
	}
	return count
}

var (
	gateSectionHeaders = [...]string{
		"Overview", "Background", "Core Concepts", "Analysis",
		"Methodology", "Evidence", "Discussion", "Implications",
	}
	gateSectionBodies = [...]string{
		"This area of %s examines how various factors influence outcomes in domain %d. " +
			"Researchers have identified several key variables that predict performance " +
			"across different contexts and populations.",
		"Historical development of %s shows a progression from early theories to modern " +
			"frameworks addressing topic %d. Early work established foundational principles " +
			"that continue to shape current understanding.",
		"Central theories in %s applicable to question %d involve multi-factor models. " +
			"These models account for interactions between primary and secondary variables, " +
			"yielding predictions that can be tested empirically.",
		"Quantitative methods in %s applied to topic %d reveal statistically significant " +
			"patterns. Meta-analyses across multiple studies confirm the robustness and " +
			"generalizability of these findings.",
		"Empirical approaches to %s for investigating topic %d use controlled experiments " +
			"and longitudinal studies. Sample sizes typically range from hundreds to thousands " +
			"of observations, ensuring adequate statistical power.",
		"Data collected across studies of %s concerning aspect %d support the theoretical " +
			"predictions. Effect sizes range from small to moderate, with variability " +
			"attributable to methodological differences.",
		"Competing perspectives on %s regarding question %d offer complementary explanations. " +
			"Integration of these viewpoints yields a more complete understanding of the " +
			"underlying mechanisms.",
		"Broader implications of %s findings on topic %d extend to policy and practice. " +
			"Translation from theory to application requires careful consideration of " +
			"contextual factors and implementation constraints.",
	}
	gateQuestions = [...]string{
		"Which factor most strongly influences the primary outcome variable?",
		"What is the relationship between the independent and dependent variables?",
		"How does the control condition differ from the experimental condition?",
		"Which theoretical framework best explains the observed phenomenon?",
	}
	gateOptions = [...][4]string{
		{"Environmental factors dominate", "Genetic predisposition is primary",
			"Interaction effects are strongest", "No single factor predominates"},
		{"Positive linear correlation", "Inverse relationship",
			"Curvilinear association", "No significant relationship"},
		{"Magnitude of effect differs", "Direction of effect reverses",
			"Variance is altered", "Both groups show convergence"},
		{"Classical theory", "Modern synthesis",
			"Ecological model", "Systems perspective"},
	}
)

func gateDoc(category string, num int) string {
	title := strings.ReplaceAll(category, "_", " ")
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Question %d\n\n", title, num)

	nSections := 3 + num%3
	for sec := 0; sec < nSections; sec++ {
		hi := (num + sec) % len(gateSectionHeaders)
		bi := (num + sec*3) % len(gateSectionBodies)
		fmt.Fprintf(&b, "## %s\n\n", gateSectionHeaders[hi])
		fmt.Fprintf(&b, gateSectionBodies[bi]+"\n\n", title, num)
		if (num+sec)%2 == 0 {
			extra := (num*7 + sec*13) % len(gateSectionBodies)
			fmt.Fprintf(&b, gateSectionBodies[extra]+"\n\n", title, num)
		}
	}

	qi := num % len(gateQuestions)
	oi := num % len(gateOptions)
	fmt.Fprintf(&b, "## Assessment\n\n%s\n\n", gateQuestions[qi])
	for j, opt := range gateOptions[oi] {
		fmt.Fprintf(&b, "- %s) %s\n", string(rune('A'+j)), opt)
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n", string(rune('A'+num%4)))
	return b.String()
}
