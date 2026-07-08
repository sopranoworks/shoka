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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

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
// librarian. Unlike the fsCorpus-based gate test in pkg/librarian/, this test
// exercises the actual production code path:
//
//   - librariansrc.Corpus backed by FSGitStorage (not fsCorpus)
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
	fileCount += writeGateQuality(t, s, "gate", "benchmark", 10)
	fileCount += writeGateLegal(t, s, "gate", "benchmark", 10)
	write(t, s, "gate", "benchmark", gateTargetPath, gateTargetContent)
	fileCount++
	t.Logf("Wrote %d files (MMLU + QuALITY + legalbench) to storage", fileCount)

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
	maxSteps := gateMaxSteps()
	lib := librarian.New(client, maxSteps).WithLogger(logger)
	if suffix := os.Getenv("GATE_SYSTEM_SUFFIX"); suffix != "" {
		lib.WithSystemSuffix(suffix)
		t.Logf("GATE_SYSTEM_SUFFIX=%q", suffix)
	}
	t.Logf("GATE_MAX_STEPS=%d", maxSteps)

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
	answerPreview := res.RawAnswer
	if len(answerPreview) > 300 {
		answerPreview = answerPreview[:300] + "…"
	}
	t.Logf("Answer (raw): %q", answerPreview)
	t.Logf("Calls (%d):", len(res.Calls))
	for i, c := range res.Calls {
		t.Logf("  [%d] %s path=%q refused=%v", i, c.Tool, c.Path, c.Refused)
	}

	// --- Gate assertions (judged on RAW answer, no stripping) ---
	rawLeak := gateControlTokenRe.MatchString(res.RawAnswer)
	ffaInvoked := strings.Contains(logBuf.String(), "forcing final answer")
	answerEmpty := strings.TrimSpace(res.RawAnswer) == ""
	t.Logf("GATE_RAW_LEAK=%v", rawLeak)
	t.Logf("GATE_FFA_INVOKED=%v", ffaInvoked)
	t.Logf("GATE_ANSWER_EMPTY=%v", answerEmpty)

	if answerEmpty {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
	}

	if rawLeak {
		t.Errorf("GATE FAIL: control tokens in raw answer: %q", answerPreview)
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

	// Per-element structured correctness
	norm := strings.ToLower(gateNormalizeSpaces(res.RawAnswer))
	elemDate := gateCheckElement(t, norm, "DATE", "july 3", "2026-07-03")
	elemTool := gateCheckElement(t, norm, "TOOL", "fuigo")
	elemDoc := gateCheckElement(t, norm, "DOC", "readme")
	elemCount := 0
	if elemDate {
		elemCount++
	}
	if elemTool {
		elemCount++
	}
	if elemDoc {
		elemCount++
	}
	t.Logf("GATE_CORRECTNESS=%d/3 (date=%v tool=%v doc=%v)", elemCount, elemDate, elemTool, elemDoc)
}

// --- Helpers ---

func gateEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func gateMaxSteps() int {
	if v := os.Getenv("GATE_MAX_STEPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
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

// --- Scale gate test ---

var gateScaleRan atomic.Bool

// gateScaleCategories is the full 41-category MMLU set (820 files at 20 per
// category). Used by TestLibrarianGate_Scale to exercise the production corpus
// at realistic haystack size.
var gateScaleCategories = [...]string{
	"abstract_algebra", "anatomy", "astronomy", "business_ethics",
	"clinical_knowledge", "college_biology", "college_chemistry",
	"college_computer_science", "college_mathematics", "college_medicine",
	"college_physics", "computer_security", "conceptual_physics",
	"econometrics", "electrical_engineering", "elementary_mathematics",
	"formal_logic", "global_facts", "high_school_biology",
	"high_school_chemistry", "high_school_computer_science",
	"high_school_european_history", "high_school_geography",
	"high_school_government_and_politics", "high_school_macroeconomics",
	"high_school_mathematics", "high_school_microeconomics",
	"high_school_physics", "high_school_psychology",
	"high_school_statistics", "high_school_us_history",
	"high_school_world_history", "human_aging", "human_sexuality",
	"international_law", "jurisprudence", "logical_fallacies",
	"machine_learning", "management", "marketing",
	"medical_genetics",
}

// TestLibrarianGate_Scale is the 820-file production-path gate test.
// Same as TestLibrarianGate_ProductionPath but at full MMLU scale (41 categories
// × 20 questions). Uses librariansrc.Corpus backed by FSGitStorage with bigram
// fulltext index + vector similarity search — the exact production code path.
func TestLibrarianGate_Scale(t *testing.T) {
	if !gateScaleRan.CompareAndSwap(false, true) {
		t.Skip("scale gate already exercised once in this process")
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
	project(t, s, "gate", "scale")

	fileCount := writeGateCorpusScale(t, s)
	if fileCount < 800 {
		t.Fatalf("expected 800+ corpus files, got %d", fileCount)
	}
	fileCount += writeGateQuality(t, s, "gate", "scale", 20)
	fileCount += writeGateLegal(t, s, "gate", "scale", 20)
	write(t, s, "gate", "scale", gateTargetPath, gateTargetContent)
	fileCount++
	t.Logf("Wrote %d files (MMLU + QuALITY + legalbench) to storage", fileCount)

	if !s.WaitForWAL(120 * time.Second) {
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
			vectorActive = gateWaitEmbeddings(t, s, fileCount, 180*time.Second)
			t.Logf("Vector index: active=%v", vectorActive)
		} else {
			t.Logf("Embedding probe failed (%v); proceeding without vector index", probeErr)
			embedder = nil
		}
	} else {
		t.Logf("Embedder creation failed (%v); proceeding without vector index", embedErr)
	}

	// --- Production corpus ---
	corpus := NewCorpus(s, "gate", "scale").
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
	root, rootErr := s.ProjectPath("gate", "scale")
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
	answerPreview := res.RawAnswer
	if len(answerPreview) > 300 {
		answerPreview = answerPreview[:300] + "…"
	}
	t.Logf("Answer (raw): %q", answerPreview)
	t.Logf("Calls (%d):", len(res.Calls))
	for i, c := range res.Calls {
		t.Logf("  [%d] %s path=%q refused=%v", i, c.Tool, c.Path, c.Refused)
	}

	// --- Gate assertions ---
	rawLeak := gateControlTokenRe.MatchString(res.RawAnswer)
	ffaInvoked := strings.Contains(logBuf.String(), "forcing final answer")
	answerEmpty := strings.TrimSpace(res.RawAnswer) == ""
	t.Logf("GATE_RAW_LEAK=%v", rawLeak)
	t.Logf("GATE_FFA_INVOKED=%v", ffaInvoked)
	t.Logf("GATE_ANSWER_EMPTY=%v", answerEmpty)

	if answerEmpty {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
	}

	if rawLeak {
		t.Errorf("GATE FAIL: control tokens in raw answer: %q", answerPreview)
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

func writeGateCorpusScale(t *testing.T, s *storage.FSGitStorage) int {
	t.Helper()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	count := 0

	for _, cat := range gateScaleCategories {
		rows, err := gateDownloadCategory(httpClient, cat)
		if err != nil {
			t.Skipf("MMLU download failed for %s: %v", cat, err)
		}
		for i, row := range rows {
			path := fmt.Sprintf("benchmark/%s/q%03d.md", cat, i+1)
			content := gateMMluDoc(cat, i+1, row)
			write(t, s, "gate", "scale", path, content)
			count++
		}
	}
	return count
}

// --- MMLU corpus download ---
//
// Downloads actual MMLU benchmark questions (MIT license) from HuggingFace
// at test time. Used by both the 6-category production path test and the
// 41-category scale test.

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
	if rows, ok := gateLoadMMLUCache(category); ok {
		return rows, nil
	}

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
	gateSaveMMLUCache(category, rows)
	return rows, nil
}

func gateMMLUCacheDir() string {
	return filepath.Join(os.TempDir(), "shoka-mmlu-cache")
}

func gateLoadMMLUCache(category string) ([]gateMMLURow, bool) {
	path := filepath.Join(gateMMLUCacheDir(), category+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rows []gateMMLURow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, false
	}
	return rows, len(rows) > 0
}

func gateSaveMMLUCache(category string, rows []gateMMLURow) {
	dir := gateMMLUCacheDir()
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.Marshal(rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, category+".json"), data, 0o644)
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

// --- QuALITY corpus download (long-form articles, CC-BY-4.0 license) ---
//
// Downloads from emozilla/quality on HuggingFace. Articles are long-form
// prose (fiction, essays, ~5K-30K chars) — much closer to real Shoka
// documentation than MMLU's short Q&A format.

type gateQualityRow struct {
	Article  string   `json:"article"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Answer   int      `json:"answer"`
}

type gateQualityResponse struct {
	Rows []struct {
		Row gateQualityRow `json:"row"`
	} `json:"rows"`
}

func gateDownloadQuality(client *http.Client, count int) ([]gateQualityRow, error) {
	if rows, ok := gateLoadQualityCache(count); ok {
		return rows, nil
	}

	url := fmt.Sprintf(
		"https://datasets-server.huggingface.co/rows?dataset=emozilla/quality&config=default&split=validation&offset=0&length=%d",
		count,
	)
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET quality: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET quality: HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read quality: %w", err)
	}

	var data gateQualityResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse quality: %w", err)
	}

	rows := make([]gateQualityRow, 0, len(data.Rows))
	for _, r := range data.Rows {
		rows = append(rows, r.Row)
	}
	gateSaveQualityCache(count, rows)
	return rows, nil
}

func gateQualityCacheDir() string {
	return filepath.Join(os.TempDir(), "shoka-quality-cache")
}

func gateLoadQualityCache(count int) ([]gateQualityRow, bool) {
	path := filepath.Join(gateQualityCacheDir(), fmt.Sprintf("quality-%d.json", count))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rows []gateQualityRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, false
	}
	return rows, len(rows) > 0
}

func gateSaveQualityCache(count int, rows []gateQualityRow) {
	dir := gateQualityCacheDir()
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.Marshal(rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("quality-%d.json", count)), data, 0o644)
}

func gateQualityDoc(num int, row gateQualityRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Long-Form Article %d\n\n", num)
	b.WriteString(row.Article)
	b.WriteString("\n\n---\n\n")
	fmt.Fprintf(&b, "**Comprehension Question:** %s\n\n", row.Question)
	for i, opt := range row.Options {
		if i < 4 {
			fmt.Fprintf(&b, "- %s) %s\n", gateAnswerLetters[i], opt)
		}
	}
	answer := "A"
	if row.Answer >= 0 && row.Answer < 4 {
		answer = gateAnswerLetters[row.Answer]
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n", answer)
	return b.String()
}

func writeGateQuality(t *testing.T, s *storage.FSGitStorage, ns, proj string, count int) int {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	rows, err := gateDownloadQuality(client, count)
	if err != nil {
		t.Logf("QuALITY download failed (non-fatal, skipping source): %v", err)
		return 0
	}
	for i, row := range rows {
		path := fmt.Sprintf("benchmark/quality/article%03d.md", i+1)
		content := gateQualityDoc(i+1, row)
		write(t, s, ns, proj, path, content)
	}
	t.Logf("QuALITY: wrote %d long-form articles", len(rows))
	return len(rows)
}

// --- legalbench corpus download (legal/corporate text, CC-BY-4.0 license) ---
//
// Downloads from nguha/legalbench corporate_lobbying config on HuggingFace.
// Documents contain legislative bill summaries and corporate descriptions
// (~5-7K chars each) — structured prose with sections, closer to real
// project documentation than MMLU.

type gateLegalRow struct {
	BillTitle          string `json:"bill_title"`
	BillSummary        string `json:"bill_summary"`
	CompanyName        string `json:"company_name"`
	CompanyDescription string `json:"company_description"`
	Answer             string `json:"answer"`
}

type gateLegalResponse struct {
	Rows []struct {
		Row gateLegalRow `json:"row"`
	} `json:"rows"`
}

func gateDownloadLegal(client *http.Client, count int) ([]gateLegalRow, error) {
	if rows, ok := gateLoadLegalCache(count); ok {
		return rows, nil
	}

	url := fmt.Sprintf(
		"https://datasets-server.huggingface.co/rows?dataset=nguha/legalbench&config=corporate_lobbying&split=test&offset=0&length=%d",
		count,
	)
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET legalbench: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET legalbench: HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read legalbench: %w", err)
	}

	var data gateLegalResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse legalbench: %w", err)
	}

	rows := make([]gateLegalRow, 0, len(data.Rows))
	for _, r := range data.Rows {
		rows = append(rows, r.Row)
	}
	gateSaveLegalCache(count, rows)
	return rows, nil
}

func gateLegalCacheDir() string {
	return filepath.Join(os.TempDir(), "shoka-legal-cache")
}

func gateLoadLegalCache(count int) ([]gateLegalRow, bool) {
	path := filepath.Join(gateLegalCacheDir(), fmt.Sprintf("legal-%d.json", count))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rows []gateLegalRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, false
	}
	return rows, len(rows) > 0
}

func gateSaveLegalCache(count int, rows []gateLegalRow) {
	dir := gateLegalCacheDir()
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.Marshal(rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("legal-%d.json", count)), data, 0o644)
}

func gateLegalDoc(num int, row gateLegalRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Legislative Analysis %d\n\n", num)
	fmt.Fprintf(&b, "## Bill: %s\n\n", row.BillTitle)
	fmt.Fprintf(&b, "### Summary\n\n%s\n\n", row.BillSummary)
	fmt.Fprintf(&b, "## Company: %s\n\n", row.CompanyName)
	fmt.Fprintf(&b, "### Corporate Profile\n\n%s\n\n", row.CompanyDescription)
	fmt.Fprintf(&b, "**Lobbying Position:** %s\n", row.Answer)
	return b.String()
}

func writeGateLegal(t *testing.T, s *storage.FSGitStorage, ns, proj string, count int) int {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	rows, err := gateDownloadLegal(client, count)
	if err != nil {
		t.Logf("legalbench download failed (non-fatal, skipping source): %v", err)
		return 0
	}
	for i, row := range rows {
		path := fmt.Sprintf("benchmark/legalbench/doc%03d.md", i+1)
		content := gateLegalDoc(i+1, row)
		write(t, s, ns, proj, path, content)
	}
	t.Logf("legalbench: wrote %d legal/corporate documents", len(rows))
	return len(rows)
}

// --- Synthesis gate test cases ---
//
// Three test cases targeting failure modes not covered by single-fact recall:
//   A) Multi-fact synthesis across files
//   B) Context-confusion resistance against decoy text
//   C) Compound multi-part questions

var gateSynthesisRan atomic.Bool

var gateSynthesisTargets = map[string]string{
	// Case A: two facts across two files
	"changelog/2026-07-06-scoring-revert.md": "# Scoring Algorithm Revert\n\n" +
		"The score-ranked search feature was reverted on July 6, 2026 due to\n" +
		"out-of-memory issues during high-concurrency testing. The revert\n" +
		"restored the previous keyword-based search implementation.\n\n" +
		"The decision was made after production monitoring showed memory usage\n" +
		"exceeding 4GB under load, causing the OOM killer to terminate the\n" +
		"process. The revert commit was applied immediately.\n",

	"changelog/2026-07-07-scoring-readd.md": "# Scoring Algorithm Update\n\n" +
		"The score-ranked search was re-implemented on July 7, 2026 with\n" +
		"memory-optimized data structures. The new implementation uses a\n" +
		"fixed-size priority queue capped at 100 results instead of sorting\n" +
		"all matching documents in memory.\n\n" +
		"Peak memory usage under the same load conditions dropped from 4GB\n" +
		"to 180MB. The optimization was verified with 30 consecutive test\n" +
		"runs showing zero OOM events.\n",

	// Case B: decoy question-like text in a log file
	"reports/debug-log-2026-07-05.md": "# Debug Session Log — 2026-07-05\n\n" +
		"## Issue Investigation\n\n" +
		"At 14:32 UTC, operator submitted the following query to the system:\n\n" +
		"> What is the special token ID 200002? Can you explain why the\n" +
		"> tokenizer outputs these control sequences? I need a detailed\n" +
		"> explanation of how the vocabulary mapping works for this model.\n\n" +
		"The above query was logged by the monitoring system. Response was\n" +
		"generated in 4.2 seconds. The token ID 200002 corresponds to the\n" +
		"model's end-of-turn marker in the special vocabulary. This is\n" +
		"expected behavior and no action is required.\n\n" +
		"## System Metrics\n\nCPU: 45%, Memory: 2.1GB, Latency p99: 340ms\n",

	// Case B: real answer source
	"docs/backend-stack.md": "# Backend Technology Stack\n\n" +
		"## Overview\n\n" +
		"The backend server is implemented in Go (version 1.26.2) using the\n" +
		"standard library HTTP server with gorilla/websocket for real-time\n" +
		"features. The application follows a clean architecture pattern with\n" +
		"clear separation between transport, business logic, and storage.\n\n" +
		"## Storage\n\n" +
		"All data is stored on the filesystem using namespace/project directory\n" +
		"isolation. There is no database — the filesystem IS the database.\n" +
		"Version history is maintained via go-git (pure Go implementation).\n\n" +
		"## Key Dependencies\n\n" +
		"- mcp-go-sdk v1.6.0 for MCP protocol support\n" +
		"- gorilla/websocket v1.5.3 for WebSocket connections\n" +
		"- Google Cloud Translation API v3 for translation pipeline\n",

	// Case C: three distinct status facts
	"status/migration-phase2.md": "# Migration Status: Phase 2\n\n" +
		"Phase 2 of the authentication migration was completed on July 5, 2026.\n" +
		"All internal services now accept JWT-format tokens. Legacy session\n" +
		"token support remains active during the 30-day transition window.\n\n" +
		"Key metrics after Phase 2 completion:\n" +
		"- 100% of new sessions issue JWT tokens\n" +
		"- 12% of active sessions still hold legacy tokens\n" +
		"- Zero authentication errors reported post-migration\n",

	"status/vector-rebuild.md": "# Vector Index Rebuild Status\n\n" +
		"The vector index rebuild is currently in progress as of July 7, 2026.\n" +
		"Approximately 60% of documents have been re-indexed with the updated\n" +
		"nomic-embed-text-v1.5 embedding model.\n\n" +
		"Progress: 4,200 of 7,000 documents re-indexed\n" +
		"Estimated completion: July 9, 2026\n" +
		"Embedding rate: ~500 documents/hour\n",

	"status/auth-review.md": "# Auth Module Code Review\n\n" +
		"The code review for the new OAuth 2.0 authentication module was\n" +
		"approved and merged on July 4, 2026. All three reviewers signed off\n" +
		"with no blocking comments. Minor style suggestions were addressed\n" +
		"in follow-up commits.\n\n" +
		"The module is ready for production deployment pending completion\n" +
		"of the Phase 2 migration.\n",
}

func TestLibrarianGate_SynthesisCases(t *testing.T) {
	if !gateSynthesisRan.CompareAndSwap(false, true) {
		t.Skip("synthesis cases already exercised once in this process")
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

	s := newStore(t)
	project(t, s, "gate", "synthesis")

	// Mixed corpus: MMLU (6 categories) + QuALITY + legalbench
	httpClient := &http.Client{Timeout: 30 * time.Second}
	fileCount := 0
	for _, cat := range gateCategories {
		rows, err := gateDownloadCategory(httpClient, cat)
		if err != nil {
			t.Skipf("MMLU download failed for %s: %v", cat, err)
		}
		for i, row := range rows {
			path := fmt.Sprintf("benchmark/%s/q%03d.md", cat, i+1)
			write(t, s, "gate", "synthesis", path, gateMMluDoc(cat, i+1, row))
			fileCount++
		}
	}
	fileCount += writeGateQuality(t, s, "gate", "synthesis", 10)
	fileCount += writeGateLegal(t, s, "gate", "synthesis", 10)

	// Synthesis-specific target files
	excludeDecoy := os.Getenv("GATE_EXCLUDE_DECOY") == "1"
	for path, content := range gateSynthesisTargets {
		if excludeDecoy && path == "reports/debug-log-2026-07-05.md" {
			t.Log("GATE_EXCLUDE_DECOY=1: skipping decoy file")
			continue
		}
		write(t, s, "gate", "synthesis", path, content)
		fileCount++
	}
	t.Logf("Wrote %d files (mixed corpus + synthesis targets) to storage", fileCount)

	if !s.WaitForWAL(120 * time.Second) {
		t.Fatal("WAL drain timeout")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	// Vector index (best-effort)
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
			vectorActive = gateWaitEmbeddings(t, s, fileCount, 180*time.Second)
			t.Logf("Vector index: active=%v", vectorActive)
		} else {
			t.Logf("Embedding probe failed (%v); proceeding without vector index", probeErr)
			embedder = nil
		}
	} else {
		t.Logf("Embedder creation failed (%v); proceeding without vector index", embedErr)
	}

	// LLM client (shared across subtests)
	client, clientErr := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    llmModel,
	})
	if clientErr != nil {
		t.Fatalf("NewClient: %v", clientErr)
	}

	root, rootErr := s.ProjectPath("gate", "synthesis")
	if rootErr != nil {
		t.Fatalf("ProjectPath: %v", rootErr)
	}

	runSynthesisCase := func(t *testing.T, question string) string {
		t.Helper()

		corpus := NewCorpus(s, "gate", "synthesis").
			WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if vectorActive {
			corpus.WithVectorSearch(s)
		}
		if embedder != nil {
			corpus.WithChunkFilter(gateEmbedAdapter{embedder}, question)
		}

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		lib := librarian.New(client, gateMaxSteps()).WithLogger(logger)

		res, askErr := lib.Ask(ctx, librarian.Request{
			Question:       question,
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

		answerPreview := res.RawAnswer
		if len(answerPreview) > 500 {
			answerPreview = answerPreview[:500] + "…"
		}
		t.Logf("Answer (raw): %q", answerPreview)
		t.Logf("Calls (%d):", len(res.Calls))
		for i, c := range res.Calls {
			t.Logf("  [%d] %s path=%q refused=%v", i, c.Tool, c.Path, c.Refused)
		}

		rawLeak := gateControlTokenRe.MatchString(res.RawAnswer)
		ffaInvoked := strings.Contains(logBuf.String(), "forcing final answer")
		answerEmpty := strings.TrimSpace(res.RawAnswer) == ""
		t.Logf("GATE_RAW_LEAK=%v", rawLeak)
		t.Logf("GATE_FFA_INVOKED=%v", ffaInvoked)
		t.Logf("GATE_ANSWER_EMPTY=%v", answerEmpty)

		if answerEmpty {
			t.Logf("Debug log:\n%s", logBuf.String())
			t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
		}
		if rawLeak {
			t.Errorf("GATE FAIL: control tokens in raw answer: %q", answerPreview)
		}

		return res.RawAnswer
	}

	// --- Case A: Multi-fact synthesis ---
	t.Run("MultiFact", func(t *testing.T) {
		answer := runSynthesisCase(t,
			"What changes were made to the scoring algorithm between July 5 and July 8, 2026? Include both the revert and the re-implementation.")

		norm := strings.ToLower(gateNormalizeSpaces(answer))
		elemRevert := gateCheckElement(t, norm, "REVERT", "revert", "rolled back", "removed", "undone")
		elemRevertDate := gateCheckElement(t, norm, "REVERT_DATE", "july 6", "2026-07-06")
		elemOOM := gateCheckElement(t, norm, "REVERT_REASON", "oom", "out-of-memory", "out of memory", "memory")
		elemReimpl := gateCheckElement(t, norm, "REIMPL", "re-implement", "re-added", "updated", "new implementation", "optimiz", "priority queue")
		elemReimplDate := gateCheckElement(t, norm, "REIMPL_DATE", "july 7", "2026-07-07")
		elemCount := 0
		for _, ok := range []bool{elemRevert, elemRevertDate, elemOOM, elemReimpl, elemReimplDate} {
			if ok {
				elemCount++
			}
		}
		t.Logf("GATE_CORRECTNESS=%d/5 (revert=%v revert_date=%v oom=%v reimpl=%v reimpl_date=%v)",
			elemCount, elemRevert, elemRevertDate, elemOOM, elemReimpl, elemReimplDate)

		if !elemRevert {
			t.Errorf("GATE FAIL MultiFact: answer does not mention the revert.\nFull answer: %q", answer)
		}
		if !elemReimpl {
			t.Errorf("GATE FAIL MultiFact: answer does not mention the re-implementation.\nFull answer: %q", answer)
		}
	})

	// --- Case B: Context-confusion resistance ---
	t.Run("ContextConfusion", func(t *testing.T) {
		answer := runSynthesisCase(t,
			"What programming language and version is the backend server written in?")

		norm := strings.ToLower(gateNormalizeSpaces(answer))
		elemLang := gateCheckElement(t, norm, "LANGUAGE", "go", "golang")
		elemVersion := gateCheckElement(t, norm, "VERSION", "1.26.2")
		elemDecoyAbsent := !gateContainsAny(norm, "200002", "tokenizer", "vocabulary mapping", "control sequences")
		t.Logf("GATE_ELEM_DECOY_ABSENT=%v", elemDecoyAbsent)
		elemCount := 0
		if elemLang {
			elemCount++
		}
		if elemVersion {
			elemCount++
		}
		if elemDecoyAbsent {
			elemCount++
		}
		t.Logf("GATE_CORRECTNESS=%d/3 (language=%v version=%v decoy_absent=%v)",
			elemCount, elemLang, elemVersion, elemDecoyAbsent)

		if !elemLang {
			t.Errorf("GATE FAIL ContextConfusion: answer does not mention Go.\nFull answer: %q", answer)
		}
		if !elemDecoyAbsent {
			t.Errorf("GATE FAIL ContextConfusion: answer was confused by decoy text — mentions tokenizer/vocabulary content.\nFull answer: %q", answer)
		}
	})

	// --- Case C: Compound multi-part query ---
	t.Run("CompoundQuery", func(t *testing.T) {
		answer := runSynthesisCase(t,
			"What is the current status of each: (1) the authentication migration, (2) the vector index rebuild, and (3) the auth module code review?")

		norm := strings.ToLower(gateNormalizeSpaces(answer))
		elemMigration := gateCheckElement(t, norm, "MIGRATION", "completed", "complete", "finished", "done", "phase 2")
		elemRebuild := gateCheckElement(t, norm, "REBUILD", "in progress", "60%", "progress", "re-index", "rebuild")
		elemReview := gateCheckElement(t, norm, "REVIEW", "approved", "merged", "signed off", "ready")
		topics := 0
		if elemMigration {
			topics++
		}
		if elemRebuild {
			topics++
		}
		if elemReview {
			topics++
		}
		t.Logf("GATE_CORRECTNESS=%d/3 (migration=%v rebuild=%v review=%v)",
			topics, elemMigration, elemRebuild, elemReview)

		if !elemMigration {
			t.Errorf("GATE FAIL CompoundQuery: answer does not mention migration completion.\nFull answer: %q", answer)
		}
		if !elemRebuild {
			t.Errorf("GATE FAIL CompoundQuery: answer does not mention vector rebuild in-progress status.\nFull answer: %q", answer)
		}
		if !elemReview {
			t.Errorf("GATE FAIL CompoundQuery: answer does not mention code review approval.\nFull answer: %q", answer)
		}
	})
}

func gateContainsAny(s string, alternatives ...string) bool {
	for _, alt := range alternatives {
		if strings.Contains(s, alt) {
			return true
		}
	}
	return false
}

// gateNormalizeSpaces replaces all Unicode whitespace (U+202F, U+00A0, etc.)
// with ASCII space so keyword matching works regardless of model output encoding.
func gateNormalizeSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
}

func TestGateNormalizeSpaces(t *testing.T) {
	// U+202F NARROW NO-BREAK SPACE — the exact character gpt-oss-20b emits
	input := "July 3, 2026"
	got := gateNormalizeSpaces(input)
	want := "July 3, 2026"
	if got != want {
		t.Errorf("gateNormalizeSpaces(%q) = %q, want %q", input, got, want)
	}

	// U+00A0 NO-BREAK SPACE
	input2 := "Go 1.26.2"
	got2 := gateNormalizeSpaces(input2)
	want2 := "Go 1.26.2"
	if got2 != want2 {
		t.Errorf("gateNormalizeSpaces(%q) = %q, want %q", input2, got2, want2)
	}
}

// gateCheckElement logs a per-element correctness result and returns whether
// the element was found.
func gateCheckElement(t *testing.T, normalized string, name string, alternatives ...string) bool {
	t.Helper()
	found := gateContainsAny(normalized, alternatives...)
	t.Logf("GATE_ELEM_%s=%v", name, found)
	return found
}
