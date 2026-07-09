package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlaywrightE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Playwright E2E in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}

	root := repoRoot(t)
	webDir := filepath.Join(root, "web")

	pwVersion := readPWVersion(t, webDir)
	image := "mcr.microsoft.com/playwright:v" + pwVersion + "-noble"

	// Copy the source tree to a temp directory so Docker never writes
	// root-owned files into the host's working tree.  We use
	// os.MkdirTemp (not t.TempDir) because Docker may create files
	// under a userns-remapped uid that the host Go process cannot
	// remove.  Cleanup runs `rm -rf` inside a container that shares
	// the same uid namespace as the test container, then removes the
	// empty parent from the host.
	tmpParent, err := os.MkdirTemp("", "TestPlaywrightE2E")
	if err != nil {
		t.Fatal(err)
	}
	tmpRoot := filepath.Join(tmpParent, "shoka")
	copyTree(t, root, tmpRoot)

	resultParent, err := os.MkdirTemp("", "TestPlaywrightE2EResults")
	if err != nil {
		t.Fatal(err)
	}
	resultDir := resultParent

	t.Cleanup(func() {
		for _, dir := range []string{tmpRoot, resultDir} {
			rm := exec.Command("docker", "run", "--rm",
				"-v", dir+":"+dir,
				"alpine", "rm", "-rf", dir)
			if out, err := rm.CombinedOutput(); err != nil {
				t.Logf("docker rm cleanup %s: %v\n%s", dir, err, out)
			}
		}
		os.RemoveAll(tmpParent)
		os.RemoveAll(resultParent)
	})

	goroot := runtime.GOROOT()
	gopath := envOr("GOPATH", filepath.Join(home(), "go"))
	goPkgDir := filepath.Dir(filepath.Dir(goroot))
	goBuildCache := filepath.Join(home(), ".cache", "go-build")

	port := envOr("SHOKA_E2E_PORT", "8099")

	args := []string{
		"run", "--rm",
		"--ipc=host", "--network=host",
		"-v", tmpRoot + ":" + tmpRoot,
		"-v", goroot + ":" + goroot + ":ro",
		"-v", goPkgDir + ":" + goPkgDir + ":ro",
		"-v", gopath + ":/root/go",
		"-v", goBuildCache + ":/root/.cache/go-build",
		"-v", resultDir + ":/results",
		"-w", filepath.Join(tmpRoot, "web"),
		"-e", "PATH=" + goroot + "/bin:/usr/local/bin:/usr/bin:/bin",
		"-e", "GOROOT=" + goroot,
		"-e", "GOPATH=/root/go",
		"-e", "GOFLAGS=-buildvcs=false",
		"-e", "HOME=/root",
		"-e", "SHOKA_E2E_PORT=" + port,
		"-e", "PLAYWRIGHT_JSON_OUTPUT_FILE=/results/report.json",
		image,
		"sh", "-c", "npm run build:nocheck && npx playwright test",
	}

	cmd := exec.Command("docker", args...)
	output, dockerErr := cmd.CombinedOutput()
	if len(output) > 0 {
		t.Log(string(output))
	}

	reportPath := filepath.Join(resultDir, "report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		if dockerErr != nil {
			t.Fatalf("playwright did not run: %v", dockerErr)
		}
		t.Fatalf("json report not found: %v", err)
	}

	results := parseReport(t, data)
	if len(results) == 0 {
		t.Fatal("no tests in playwright report")
	}

	for _, r := range results {
		t.Run(r.name, func(t *testing.T) {
			if r.skipped {
				t.Skip("skipped by playwright")
			}
			if !r.passed {
				t.Fatal(strings.Join(r.errors, "\n"))
			}
		})
	}
}

// --- helpers ---

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("rsync", "-a", "--exclude=.git", "--exclude=playwright-failures", src+"/", dst+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rsync source tree to temp: %v\n%s", err, out)
	}
	fixAbsSymlinks(t, src, dst)
}

// fixAbsSymlinks rewrites absolute symlinks that point into the original source
// tree so they resolve inside the temp copy. npm workspaces create these for
// cross-package hoisted dependencies (e.g. packages/web-core/node_modules/foo
// -> /original/root/web/node_modules/foo).
func fixAbsSymlinks(t *testing.T, origRoot, newRoot string) {
	t.Helper()
	prefix := origRoot + "/"
	fixed := 0
	_ = filepath.WalkDir(newRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil || !strings.HasPrefix(target, prefix) {
			return nil
		}
		newTarget := newRoot + target[len(origRoot):]
		_ = os.Remove(path)
		if err := os.Symlink(newTarget, path); err != nil {
			t.Logf("fixAbsSymlinks: could not rewrite %s: %v", path, err)
		}
		fixed++
		return nil
	})
	if fixed > 0 {
		t.Logf("fixAbsSymlinks: rewrote %d absolute symlinks", fixed)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

func readPWVersion(t *testing.T, webDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(webDir, "node_modules", "@playwright", "test", "package.json"))
	if err != nil {
		t.Fatalf("cannot read playwright version (run npm install first): %v", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	return pkg.Version
}

func home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- Playwright JSON report types ---

type pwReport struct {
	Suites []pwSuite `json:"suites"`
}

type pwSuite struct {
	Title  string    `json:"title"`
	Suites []pwSuite `json:"suites"`
	Specs  []pwSpec  `json:"specs"`
}

type pwSpec struct {
	Title string   `json:"title"`
	OK    bool     `json:"ok"`
	Tests []pwTest `json:"tests"`
}

type pwTest struct {
	Status  string      `json:"status"`
	Results []pwTestRun `json:"results"`
}

type pwTestRun struct {
	Status string    `json:"status"`
	Errors []pwError `json:"errors"`
}

type pwError struct {
	Message string `json:"message"`
}

type testResult struct {
	name    string
	passed  bool
	skipped bool
	errors  []string
}

func parseReport(t *testing.T, data []byte) []testResult {
	t.Helper()
	data = bytes.TrimLeft(data, "\xef\xbb\xbf")
	var report pwReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("failed to parse playwright json report: %v", err)
	}
	return flattenSuites(report.Suites, "")
}

func flattenSuites(suites []pwSuite, prefix string) []testResult {
	var results []testResult
	for _, s := range suites {
		p := s.Title
		if prefix != "" {
			p = fmt.Sprintf("%s > %s", prefix, s.Title)
		}
		for _, spec := range s.Specs {
			r := testResult{
				name: fmt.Sprintf("%s > %s", p, spec.Title),
			}
			if len(spec.Tests) > 0 {
				test := spec.Tests[0]
				r.skipped = test.Status == "skipped"
				r.passed = spec.OK
				if !r.passed && len(test.Results) > 0 {
					for _, e := range test.Results[0].Errors {
						r.errors = append(r.errors, e.Message)
					}
				}
			}
			results = append(results, r)
		}
		results = append(results, flattenSuites(s.Suites, p)...)
	}
	return results
}
