// gate-multimodel automates multi-model gate testing via LM Studio's load/unload API.
//
// For each tool-use-capable LLM in LM Studio, it: loads the model, runs all
// gate assertions (non-empty answer, no control tokens, target file found),
// records pass/fail, and unloads. Outputs a summary table.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

const (
	lmsBaseURL   = "http://localhost:1234"
	openaiBase   = lmsBaseURL + "/v1"
	gateQuestion = "When was the build tool's setup procedure added to the project documentation?"
)

var controlTokenRe = regexp.MustCompile(`<\|[^|]*\|>`)

// lmsModel is a model entry from the LM Studio /api/v1/models response.
type lmsModel struct {
	Key              string `json:"key"`
	Type             string `json:"type"`
	DisplayName      string `json:"display_name"`
	LoadedInstances  []any  `json:"loaded_instances"`
	Capabilities     *lmsCaps `json:"capabilities"`
}

type lmsCaps struct {
	TrainedForToolUse bool `json:"trained_for_tool_use"`
}

type lmsModelsResponse struct {
	Models []lmsModel `json:"models"`
}

// gateResult records the outcome for one model.
type gateResult struct {
	Model         string
	DisplayName   string
	NonEmpty      bool
	NoCtrlTokens  bool // post-strip answer has no control tokens (always true after strip)
	RawLeak       bool // raw answer contained control tokens before stripping
	TargetFound   bool
	Error         string
	AnswerPreview string
	Duration      time.Duration
}

func (r gateResult) pass() bool {
	return r.Error == "" && r.NonEmpty && r.NoCtrlTokens && r.TargetFound
}

func main() {
	models, err := listModels()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query LM Studio: %v\n", err)
		os.Exit(1)
	}

	var candidates []lmsModel
	for _, m := range models {
		if m.Type != "llm" {
			continue
		}
		if m.Capabilities == nil || !m.Capabilities.TrainedForToolUse {
			continue
		}
		candidates = append(candidates, m)
	}

	if len(candidates) == 0 {
		fmt.Fprintln(os.Stderr, "No tool-use-capable LLM models found in LM Studio")
		os.Exit(1)
	}

	fmt.Printf("Found %d tool-use-capable models:\n", len(candidates))
	for _, m := range candidates {
		fmt.Printf("  - %s (%s)\n", m.Key, m.DisplayName)
	}
	fmt.Println()

	root, err := os.MkdirTemp("", "gate-multimodel-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(root)

	fmt.Print("Downloading MMLU corpus...")
	count, err := downloadMMLUCorpus(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed to download corpus: %v\n", err)
		os.Exit(1)
	}

	targetPath := "changelog/2026-07-03-fuigo-install.md"
	if err := os.MkdirAll(filepath.Join(root, "changelog"), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir changelog: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(root, targetPath),
		[]byte("# Build Tool Installation\n\n"+
			"fuigo installation was added to README.md on July 3, 2026.\n\n"+
			"The setup procedure for the build tool is now documented in the\n"+
			"project's main README file, including prerequisites, installation\n"+
			"steps, and configuration options.\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write target: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(" %d files + 1 target ready.\n\n", count)

	var results []gateResult

	for i, m := range candidates {
		fmt.Printf("━━━ [%d/%d] %s (%s) ━━━\n", i+1, len(candidates), m.Key, m.DisplayName)

		fmt.Printf("  Loading model...")
		if err := loadModel(m.Key); err != nil {
			fmt.Printf(" FAILED: %v\n\n", err)
			results = append(results, gateResult{
				Model:       m.Key,
				DisplayName: m.DisplayName,
				Error:       fmt.Sprintf("load failed: %v", err),
			})
			continue
		}
		fmt.Println(" OK")

		fmt.Printf("  Running gate test...")
		result := runGate(root, m.Key, m.DisplayName)
		results = append(results, result)

		if result.Error != "" {
			fmt.Printf(" ERROR: %s\n", result.Error)
		} else if result.pass() {
			fmt.Printf(" PASS (%s)\n", result.Duration.Round(time.Millisecond))
		} else {
			fmt.Printf(" FAIL (%s)\n", result.Duration.Round(time.Millisecond))
			if !result.NonEmpty {
				fmt.Println("    ✗ empty answer")
			}
			if !result.NoCtrlTokens {
				fmt.Println("    ✗ control tokens in stripped answer")
			}
			if !result.TargetFound {
				fmt.Println("    ✗ target file not found")
			}
		}
		if result.RawLeak {
			fmt.Println("  ⚠ raw answer contained control tokens (stripped before final answer)")
		}
		if result.AnswerPreview != "" {
			fmt.Printf("  Answer: %s\n", result.AnswerPreview)
		}

		fmt.Printf("  Unloading model...")
		if err := unloadModel(m.Key); err != nil {
			fmt.Printf(" WARN: %v\n", err)
		} else {
			fmt.Println(" OK")
		}
		fmt.Println()
	}

	printSummary(results)
}

func runGate(root, modelKey, displayName string) gateResult {
	start := time.Now()
	r := gateResult{
		Model:       modelKey,
		DisplayName: displayName,
	}

	os.Setenv("OPENAI_API_KEY", "lm-studio")

	client, err := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  openaiBase,
		Model:    modelKey,
	})
	if err != nil {
		r.Error = fmt.Sprintf("NewClient: %v", err)
		r.Duration = time.Since(start)
		return r
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := librarian.New(client, 0).WithLogger(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	res, err := lib.Ask(ctx, librarian.Request{
		Question: gateQuestion,
		Root:     root,
	})
	r.Duration = time.Since(start)
	if err != nil {
		r.Error = fmt.Sprintf("Ask: %v", err)
		return r
	}

	r.RawLeak = controlTokenRe.MatchString(res.RawAnswer)

	answer := strings.TrimSpace(res.Answer)
	r.NonEmpty = answer != ""
	r.NoCtrlTokens = !controlTokenRe.MatchString(res.Answer)

	for _, c := range res.Calls {
		if c.Tool == "read" && !c.Refused && strings.Contains(c.Path, "fuigo") {
			r.TargetFound = true
			break
		}
	}

	if len(answer) > 120 {
		r.AnswerPreview = answer[:120] + "..."
	} else {
		r.AnswerPreview = answer
	}

	return r
}

func printSummary(results []gateResult) {
	fmt.Println("╔══════════════════════════════════════════════════╦══════════╦══════════╦══════════════╦══════════╦══════════╗")
	fmt.Println("║ Model                                            ║ Overall  ║ Raw leak ║ Answer valid ║ Target   ║ Duration ║")
	fmt.Println("╠══════════════════════════════════════════════════╬══════════╬══════════╬══════════════╬══════════╬══════════╣")

	passCount := 0
	leakCount := 0
	for _, r := range results {
		model := r.Model
		if len(model) > 50 {
			model = model[:47] + "..."
		}

		overall := "FAIL"
		if r.Error != "" {
			overall = "ERROR"
		} else if r.pass() {
			overall = "PASS"
			passCount++
		}

		rawLeak := "-"
		answerValid := "-"
		if r.Error == "" {
			if r.RawLeak {
				rawLeak = "YES"
				leakCount++
			} else {
				rawLeak = "no"
			}
			if r.NonEmpty && r.NoCtrlTokens {
				answerValid = "PASS"
			} else {
				answerValid = "FAIL"
			}
		}

		dur := "-"
		if r.Duration > 0 {
			dur = r.Duration.Round(time.Second).String()
		}

		fmt.Printf("║ %-48s ║ %-8s ║ %-8s ║ %-12s ║ %-8s ║ %8s ║\n",
			model,
			overall,
			rawLeak,
			answerValid,
			boolMark(r.TargetFound, r.Error != ""),
			dur,
		)
	}

	fmt.Println("╚══════════════════════════════════════════════════╩══════════╩══════════╩══════════════╩══════════╩══════════╝")
	fmt.Printf("\n%d/%d models passed all gate checks.\n", passCount, len(results))
	if leakCount > 0 {
		fmt.Printf("%d/%d models leaked raw control tokens (recovered by stripping).\n", leakCount, len(results))
	} else {
		fmt.Println("No models leaked raw control tokens.")
	}
}

func boolMark(val bool, isErr bool) string {
	if isErr {
		return "-"
	}
	if val {
		return "PASS"
	}
	return "FAIL"
}

// --- LM Studio API ---

func listModels() ([]lmsModel, error) {
	resp, err := http.Get(lmsBaseURL + "/api/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data lmsModelsResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data.Models, nil
}

func loadModel(key string) error {
	payload, _ := json.Marshal(map[string]string{"model": key})
	resp, err := http.Post(lmsBaseURL+"/api/v1/models/load", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if result.Status != "loaded" {
		return fmt.Errorf("unexpected status: %s", result.Status)
	}
	return nil
}

func unloadModel(key string) error {
	payload, _ := json.Marshal(map[string]string{"instance_id": key})
	resp, err := http.Post(lmsBaseURL+"/api/v1/models/unload", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// --- MMLU corpus download (reused from gate test) ---

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

func downloadMMLUCorpus(root string) (int, error) {
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
			return 0, fmt.Errorf("%s: %w", r.cat, r.err)
		}
	}

	count := 0
	for _, r := range results {
		dir := filepath.Join(root, "benchmark", r.cat)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, fmt.Errorf("mkdir %s: %w", dir, err)
		}
		for i, row := range r.rows {
			content := mmluDocFromRow(r.cat, i+1, row)
			path := filepath.Join(dir, fmt.Sprintf("q%03d.md", i+1))
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return 0, fmt.Errorf("write %s: %w", path, err)
			}
			count++
		}
	}
	return count, nil
}

func fetchMMLUCategory(client *http.Client, category string) ([]mmluRow, error) {
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
	return rows, nil
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
