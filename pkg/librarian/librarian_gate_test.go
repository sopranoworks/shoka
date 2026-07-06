package librarian

import (
	"bytes"
	"context"
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
	fileCount := generateBenchmarkCorpus(t, root)
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

	// --- Gate assertions ---

	// 1. Non-empty answer.
	answer := strings.TrimSpace(res.Answer)
	if answer == "" {
		t.Logf("Debug log:\n%s", logBuf.String())
		t.Errorf("GATE FAIL: empty answer with %d tool calls", len(res.Calls))
	}

	// 2. No control tokens.
	if controlTokenRe.MatchString(res.Answer) {
		t.Errorf("GATE FAIL: control tokens in answer: %q", truncate(res.Answer, 200))
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

// --- Benchmark corpus generation ---
//
// The corpus is structured after MMLU benchmark categories (MIT license).
// Each category gets its own subdirectory with 20 markdown documents, for
// a total of 820+ files. Content is generated procedurally with subject-
// specific variation — real document structure, realistic length.

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

func generateBenchmarkCorpus(t *testing.T, root string) int {
	t.Helper()
	count := 0
	for _, cat := range mmluCategories {
		dir := filepath.Join(root, "benchmark", cat)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for q := 1; q <= 20; q++ {
			content := benchmarkDoc(cat, q)
			path := filepath.Join(dir, fmt.Sprintf("q%03d.md", q))
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			count++
		}
	}
	return count
}

var (
	sectionHeaders = [...]string{
		"Overview", "Background", "Core Concepts", "Analysis",
		"Methodology", "Evidence", "Discussion", "Implications",
	}

	sectionBodies = [...]string{
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

	assessmentQuestions = [...]string{
		"Which factor most strongly influences the primary outcome variable?",
		"What is the relationship between the independent and dependent variables?",
		"How does the control condition differ from the experimental condition?",
		"Which theoretical framework best explains the observed phenomenon?",
	}

	assessmentOptions = [...][4]string{
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

func benchmarkDoc(category string, num int) string {
	title := strings.ReplaceAll(category, "_", " ")

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Question %d\n\n", title, num)

	nSections := 3 + num%3 // 3–5 body sections
	for s := 0; s < nSections; s++ {
		hi := (num + s) % len(sectionHeaders)
		bi := (num + s*3) % len(sectionBodies)
		fmt.Fprintf(&b, "## %s\n\n", sectionHeaders[hi])
		fmt.Fprintf(&b, sectionBodies[bi]+"\n\n", title, num)

		if (num+s)%2 == 0 {
			extra := (num*7 + s*13) % len(sectionBodies)
			fmt.Fprintf(&b, sectionBodies[extra]+"\n\n", title, num)
		}
	}

	qi := num % len(assessmentQuestions)
	oi := num % len(assessmentOptions)
	fmt.Fprintf(&b, "## Assessment\n\n")
	fmt.Fprintf(&b, "%s\n\n", assessmentQuestions[qi])
	for j, opt := range assessmentOptions[oi] {
		fmt.Fprintf(&b, "- %s) %s\n", string(rune('A'+j)), opt)
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n", string(rune('A'+num%4)))

	return b.String()
}
