package librarian

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// gateTestRan ensures the gate benchmark runs at most once per process.
// Like the ollama and LM Studio e2e tests, amplifying a nondeterministic
// external model call under -count=30 would manufacture flakiness.
var gateTestRan atomic.Bool

// TestAsk_GateBenchmark is the automated gate test for the librarian.
//
// It builds a production-scale corpus of 800+ markdown files (structured after
// MMLU benchmark categories, MIT license), plants one target file with known
// content, runs ask_the_librarian against LM Studio, and asserts:
//
//  1. Answer is non-empty
//  2. Answer contains no control tokens (<|...|>)
//  3. Sources include the target file
//
// This test IS the model-change gate: swap the model → run this test → pass/fail.
//
// Skip conditions: LM Studio not reachable, already ran in this process.
// Override model via LIBRARIAN_LMSTUDIO_MODEL env var.
func TestAsk_GateBenchmark(t *testing.T) {
	if !gateTestRan.CompareAndSwap(false, true) {
		t.Skip("gate benchmark already exercised once in this process")
	}

	baseURL := envOr("LIBRARIAN_LMSTUDIO_BASE_URL", defaultLMStudioBaseURL)
	model := envOr("LIBRARIAN_LMSTUDIO_MODEL", defaultLMStudioModel)

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.LastIndex(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL, err)
	}
	_ = conn.Close()

	// --- Build corpus ---
	root := t.TempDir()
	fileCount := downloadMMLUCorpus(t, root)
	if fileCount < 800 {
		t.Fatalf("expected 800+ corpus files, got %d", fileCount)
	}
	t.Logf("Generated %d benchmark corpus files in %s", fileCount, root)

	// Target file: the needle in the haystack.
	targetPath := "changelog/2026-07-03-fuigo-install.md"
	if err := os.MkdirAll(filepath.Join(root, "changelog"), 0o755); err != nil {
		t.Fatalf("mkdir changelog: %v", err)
	}
	writeFile(t, filepath.Join(root, targetPath),
		"# Build Tool Installation\n\n"+
			"fuigo installation was added to README.md on July 3, 2026.\n\n"+
			"The setup procedure for the build tool is now documented in the\n"+
			"project's main README file, including prerequisites, installation\n"+
			"steps, and configuration options.\n")

	// --- Run librarian ---
	t.Setenv("OPENAI_API_KEY", "lm-studio")
	client, clientErr := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    model,
	})
	if clientErr != nil {
		t.Fatalf("NewClient: %v", clientErr)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := New(client, 0).WithLogger(logger) // 0 → default maxSteps (12)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	res, askErr := lib.Ask(ctx, Request{
		Question: "When was the build tool's setup procedure added to the project documentation?",
		Root:     root,
	})
	if askErr != nil {
		t.Logf("Debug log:\n%s", logBuf.String())
		if strings.Contains(askErr.Error(), "connection refused") || strings.Contains(askErr.Error(), "connect:") {
			t.Skipf("LM Studio went away mid-test: %v", askErr)
		}
		t.Fatalf("Ask failed: %v", askErr)
	}

	// --- Log results ---
	t.Logf("Answer (%d chars): %q", len(res.Answer), truncate(res.Answer, 300))
	t.Logf("Tool calls (%d):", len(res.Calls))
	for i, c := range res.Calls {
		t.Logf("  [%d] %s path=%q refused=%v detail=%q", i, c.Tool, c.Path, c.Refused, c.Detail)
	}

	// --- Gate assertions (judged on RAW answer, no stripping) ---

	rawLeak := controlTokenRe.MatchString(res.RawAnswer)
	t.Logf("GATE_RAW_LEAK=%v", rawLeak)

	// 1. Non-empty answer (raw).
	answer := strings.TrimSpace(res.RawAnswer)
	if answer == "" {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
	}

	// 2. No control tokens in raw answer.
	if rawLeak {
		t.Errorf("GATE FAIL: control tokens in raw answer: %q", truncate(res.RawAnswer, 200))
	}

	// 3. Target file was accessed (via direct read or search auto-read).
	foundTarget := false
	for _, c := range res.Calls {
		if c.Tool == "read" && !c.Refused && strings.Contains(c.Path, "fuigo") {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: target file %q not in read sources", targetPath)
	}
}

// --- MMLU corpus download ---
//
// Downloads actual MMLU benchmark questions (MIT license) from HuggingFace
// at test time. 41 categories × 20 questions = 820 files.

var mmluCategories = [...]string{
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

type mmluRow struct {
	Question string   `json:"question"`
	Subject  string   `json:"subject"`
	Choices  []string `json:"choices"`
	Answer   int      `json:"answer"`
}

type mmluAPIResponse struct {
	Rows []struct {
		Row mmluRow `json:"row"`
	} `json:"rows"`
}

func downloadMMLUCorpus(t *testing.T, root string) int {
	t.Helper()

	httpClient := &http.Client{Timeout: 30 * time.Second}

	type catResult struct {
		cat  string
		rows []mmluRow
		err  error
	}
	results := make([]catResult, len(mmluCategories))

	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for i, cat := range mmluCategories {
		wg.Add(1)
		go func(idx int, category string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows, err := fetchMMLUCategory(httpClient, category)
			results[idx] = catResult{cat: category, rows: rows, err: err}
		}(i, cat)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Skipf("MMLU download failed for %s: %v", r.cat, r.err)
		}
	}

	count := 0
	for _, r := range results {
		dir := filepath.Join(root, "benchmark", r.cat)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for i, row := range r.rows {
			content := mmluDocFromRow(r.cat, i+1, row)
			path := filepath.Join(dir, fmt.Sprintf("q%03d.md", i+1))
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			count++
		}
	}
	return count
}

func fetchMMLUCategory(client *http.Client, category string) ([]mmluRow, error) {
	if rows, ok := loadMMLUCache(category); ok {
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

	var data mmluAPIResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", category, err)
	}

	rows := make([]mmluRow, 0, len(data.Rows))
	for _, r := range data.Rows {
		rows = append(rows, r.Row)
	}
	saveMMLUCache(category, rows)
	return rows, nil
}

func mmluCacheDir() string {
	return filepath.Join(os.TempDir(), "shoka-mmlu-cache")
}

func loadMMLUCache(category string) ([]mmluRow, bool) {
	path := filepath.Join(mmluCacheDir(), category+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rows []mmluRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, false
	}
	return rows, len(rows) > 0
}

func saveMMLUCache(category string, rows []mmluRow) {
	dir := mmluCacheDir()
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.Marshal(rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, category+".json"), data, 0o644)
}

var answerLetters = [...]string{"A", "B", "C", "D"}

func mmluDocFromRow(category string, num int, row mmluRow) string {
	title := strings.ReplaceAll(category, "_", " ")

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Question %d\n\n", title, num)
	fmt.Fprintf(&b, "%s\n\n", row.Question)
	for i, choice := range row.Choices {
		if i < 4 {
			fmt.Fprintf(&b, "- %s) %s\n", answerLetters[i], choice)
		}
	}
	letter := "A"
	if row.Answer >= 0 && row.Answer < 4 {
		letter = answerLetters[row.Answer]
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n", letter)
	return b.String()
}
