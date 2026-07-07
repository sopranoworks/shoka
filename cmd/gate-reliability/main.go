// gate-reliability runs the existing gate/E2E tests across all LM Studio
// models with automated load/unload, multiple repetitions, and multiple rounds.
//
// For each round × model: load model, run each test N times (separate processes
// to bypass the atomic.Bool once-per-process guard), record pass/fail + raw
// control token leak status, unload model. Print per-model and per-test tables.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	lmsBaseURL = "http://localhost:1234"
	repsPerRun = 5
	rounds     = 10
)

type testSpec struct {
	Name    string
	Package string
}

var tests = []testSpec{
	{"TestLibrarianGate_ProductionPath", "./internal/librariansrc/"},
	{"TestLibrarianE2E_MCP", "./tests/"},
	{"TestAsk_GateBenchmark", "./pkg/librarian/"},
}

type lmsModel struct {
	Key          string   `json:"key"`
	Type         string   `json:"type"`
	DisplayName  string   `json:"display_name"`
	Capabilities *lmsCaps `json:"capabilities"`
}

type lmsCaps struct {
	TrainedForToolUse bool `json:"trained_for_tool_use"`
}

type lmsModelsResponse struct {
	Models []lmsModel `json:"models"`
}

type runResult struct {
	Round    int
	Rep      int
	Model    string
	Test     string
	Passed   bool
	Skipped  bool
	RawLeak  bool
	Duration time.Duration
	Detail   string
}

func main() {
	models, err := listModels()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query LM Studio: %v\n", err)
		os.Exit(1)
	}

	var candidates []lmsModel
	for _, m := range models {
		if m.Type != "llm" && m.Type != "" {
			if m.Type == "embedding" {
				continue
			}
		}
		if m.Type == "embedding" {
			continue
		}
		if m.Capabilities == nil || !m.Capabilities.TrainedForToolUse {
			continue
		}
		candidates = append(candidates, m)
	}

	if len(candidates) == 0 {
		fmt.Fprintln(os.Stderr, "No tool-use-capable LLM models found")
		os.Exit(1)
	}

	fmt.Printf("Models: %d | Tests: %d | Reps/run: %d | Rounds: %d\n", len(candidates), len(tests), repsPerRun, rounds)
	fmt.Printf("Total test invocations: %d\n\n", len(candidates)*len(tests)*repsPerRun*rounds)
	for _, m := range candidates {
		fmt.Printf("  - %s\n", m.Key)
	}
	for _, t := range tests {
		fmt.Printf("  - %s (%s)\n", t.Name, t.Package)
	}
	fmt.Println()

	var allResults []runResult

	for round := 1; round <= rounds; round++ {
		fmt.Printf("══════ Round %d/%d ══════\n", round, rounds)

		for _, m := range candidates {
			fmt.Printf("  ▸ %s: loading...", m.Key)
			if err := loadModel(m.Key); err != nil {
				fmt.Printf(" FAILED: %v\n", err)
				for _, t := range tests {
					for rep := 1; rep <= repsPerRun; rep++ {
						allResults = append(allResults, runResult{
							Round: round, Rep: rep, Model: m.Key, Test: t.Name,
							Detail: fmt.Sprintf("load failed: %v", err),
						})
					}
				}
				continue
			}
			fmt.Print(" OK\n")

			for _, t := range tests {
				for rep := 1; rep <= repsPerRun; rep++ {
					r := runTest(round, rep, m.Key, t)
					allResults = append(allResults, r)

					mark := "✓"
					if r.Skipped {
						mark = "⊘"
					} else if !r.Passed {
						mark = "✗"
					}
					leak := ""
					if r.RawLeak {
						leak = " LEAK"
					}
					fmt.Printf("    %s r%d.%d %s %s%s\n", mark, round, rep, t.Name, r.Duration.Round(time.Millisecond), leak)
				}
			}

			fmt.Printf("  ▸ %s: unloading...", m.Key)
			if err := unloadModel(m.Key); err != nil {
				fmt.Printf(" WARN: %v\n", err)
			} else {
				fmt.Print(" OK\n")
			}
		}
		fmt.Println()
	}

	printSummary(allResults, candidates)
}

func runTest(round, rep int, model string, t testSpec) runResult {
	r := runResult{
		Round: round,
		Rep:   rep,
		Model: model,
		Test:  t.Name,
	}

	start := time.Now()

	cmd := exec.Command("go", "test", t.Package,
		"-run", "^"+t.Name+"$",
		"-v", "-count=1", "-timeout=600s")
	cmd.Env = append(os.Environ(),
		"LIBRARIAN_LMSTUDIO_MODEL="+model,
		"LIBRARIAN_LMSTUDIO_BASE_URL=http://localhost:1234/v1",
	)

	out, err := cmd.CombinedOutput()
	r.Duration = time.Since(start)
	output := string(out)

	if strings.Contains(output, "GATE_RAW_LEAK=true") {
		r.RawLeak = true
	}

	if strings.Contains(output, "--- SKIP:") {
		r.Skipped = true
		r.Passed = true
		return r
	}

	if err != nil {
		r.Passed = false
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "GATE FAIL") || strings.Contains(line, "FAIL") {
				r.Detail = strings.TrimSpace(line)
				break
			}
		}
		if r.Detail == "" && len(lines) > 0 {
			r.Detail = lines[len(lines)-1]
		}
		return r
	}

	r.Passed = true
	return r
}

func printSummary(results []runResult, models []lmsModel) {
	fmt.Println("\n╔═══════════════════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                              RELIABILITY SUMMARY                                            ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════════════════════════════════════════╝\n")

	type modelTestKey struct {
		Model, Test string
	}
	type stats struct {
		Total, Passed, Failed, Skipped, Leaked int
	}

	byModelTest := make(map[modelTestKey]*stats)
	byModel := make(map[string]*stats)

	for _, r := range results {
		mt := modelTestKey{r.Model, r.Test}
		if byModelTest[mt] == nil {
			byModelTest[mt] = &stats{}
		}
		if byModel[r.Model] == nil {
			byModel[r.Model] = &stats{}
		}
		s := byModelTest[mt]
		ms := byModel[r.Model]
		s.Total++
		ms.Total++
		if r.Skipped {
			s.Skipped++
			ms.Skipped++
		} else if r.Passed {
			s.Passed++
			ms.Passed++
		} else {
			s.Failed++
			ms.Failed++
		}
		if r.RawLeak {
			s.Leaked++
			ms.Leaked++
		}
	}

	// Per-model × test table
	fmt.Println("Per model × test:")
	fmt.Println("┌──────────────────────────────────────────────┬────────────────────────────────────┬──────┬──────┬──────┬──────┐")
	fmt.Println("│ Model                                        │ Test                               │ Pass │ Fail │ Skip │ Leak │")
	fmt.Println("├──────────────────────────────────────────────┼────────────────────────────────────┼──────┼──────┼──────┼──────┤")

	for _, m := range models {
		for _, t := range tests {
			s := byModelTest[modelTestKey{m.Key, t.Name}]
			if s == nil {
				continue
			}
			mName := m.Key
			if len(mName) > 44 {
				mName = mName[:41] + "..."
			}
			tName := t.Name
			if len(tName) > 34 {
				tName = tName[:31] + "..."
			}
			fmt.Printf("│ %-44s │ %-34s │ %4d │ %4d │ %4d │ %4d │\n",
				mName, tName, s.Passed, s.Failed, s.Skipped, s.Leaked)
		}
	}
	fmt.Println("└──────────────────────────────────────────────┴────────────────────────────────────┴──────┴──────┴──────┴──────┘")

	// Per-model aggregate
	fmt.Println("\nPer model (aggregate):")
	fmt.Println("┌──────────────────────────────────────────────┬───────┬──────┬──────┬──────┬──────┬──────────┐")
	fmt.Println("│ Model                                        │ Total │ Pass │ Fail │ Skip │ Leak │ Pass %   │")
	fmt.Println("├──────────────────────────────────────────────┼───────┼──────┼──────┼──────┼──────┼──────────┤")

	for _, m := range models {
		s := byModel[m.Key]
		if s == nil {
			continue
		}
		mName := m.Key
		if len(mName) > 44 {
			mName = mName[:41] + "..."
		}
		effective := s.Passed + s.Failed
		pct := 0.0
		if effective > 0 {
			pct = float64(s.Passed) / float64(effective) * 100
		}
		fmt.Printf("│ %-44s │ %5d │ %4d │ %4d │ %4d │ %4d │ %6.1f%%  │\n",
			mName, s.Total, s.Passed, s.Failed, s.Skipped, s.Leaked, pct)
	}
	fmt.Println("└──────────────────────────────────────────────┴───────┴──────┴──────┴──────┴──────┴──────────┘")

	// Failures detail
	var failures []runResult
	for _, r := range results {
		if !r.Passed && !r.Skipped {
			failures = append(failures, r)
		}
	}
	if len(failures) > 0 {
		fmt.Printf("\nFailures (%d):\n", len(failures))
		for _, f := range failures {
			detail := f.Detail
			if len(detail) > 100 {
				detail = detail[:100] + "..."
			}
			fmt.Printf("  r%d.%d %-40s %-34s %s\n", f.Round, f.Rep, f.Model, f.Test, detail)
		}
	}

	// Leaks detail
	var leaks []runResult
	for _, r := range results {
		if r.RawLeak {
			leaks = append(leaks, r)
		}
	}
	if len(leaks) > 0 {
		fmt.Printf("\nRaw control token leaks (%d):\n", len(leaks))
		for _, l := range leaks {
			fmt.Printf("  r%d.%d %-40s %s\n", l.Round, l.Rep, l.Model, l.Test)
		}
	} else {
		fmt.Println("\nNo raw control token leaks detected.")
	}
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
		return fmt.Errorf("parse: %w", err)
	}
	if result.Status != "loaded" {
		return fmt.Errorf("status: %s", result.Status)
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
