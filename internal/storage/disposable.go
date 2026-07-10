package storage

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// shoka.disposable mechanism (the 2026-06-02 lost+found worker directive).
//
// A shoka.disposable file declares patterns of UNTRACKED files the lost+found
// worker is authorised to DELETE on sight (OS junk like .DS_Store that recurs no
// matter how often it is removed, where preservation has no value). It is NOT a
// .gitignore and must never be used to hide untracked files (the operator's
// standing rule): .gitignore means "git should not track this"; shoka.disposable
// means "the worker may delete this". The two are distinct concepts with distinct
// files. The pattern *syntax* reuses .gitignore format (via go-git's public
// gitignore parser) only for operator familiarity.
//
// The file lives in Shoka-managed areas, never inside a project's git tree:
//   - Shoka-wide: <base>/shoka.disposable                  (every project)
//   - namespace:  <base>/<namespace>/shoka.disposable      (every project in ns)
//   - project:    <base>/<namespace>/<project>.shoka.disposable  (one project;
//     a sibling file mirroring the <project>.project.db catalog convention)
//
// The three levels merge additively into one matcher, in the order
// wide → namespace → project. go-git's matcher is last-match-wins, so the most
// specific level has the final say and a project-level "!negation" can
// re-include a path a higher level disposed of.

// disposableWidePath is the Shoka-wide shoka.disposable file path.
func (s *FSGitStorage) disposableWidePath() string {
	return filepath.Join(s.baseDir, "shoka.disposable")
}

// disposableNamespacePath is the namespace-level shoka.disposable file path.
func (s *FSGitStorage) disposableNamespacePath(namespace string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, "shoka.disposable")
}

// disposableProjectPath is the project-level shoka.disposable file path: a
// sibling file <project>.shoka.disposable alongside the project directory (and
// the <project>.project.db catalog), never inside the project's git tree.
func (s *FSGitStorage) disposableProjectPath(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, projectName+".shoka.disposable")
}

// loadDisposablePatterns reads one shoka.disposable file, returning one
// gitignore.Pattern per non-blank, non-comment line (in file order). A missing
// file yields no patterns and no error (graceful absence is the common case, as
// these files do not exist initially).
func loadDisposablePatterns(path string) ([]gitignore.Pattern, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open shoka.disposable %q: %w", path, err)
	}
	defer f.Close()

	var patterns []gitignore.Pattern
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// domain nil: patterns apply from the project root downward.
		patterns = append(patterns, gitignore.ParsePattern(line, nil))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read shoka.disposable %q: %w", path, err)
	}
	return patterns, nil
}

// effectiveDisposable builds the merged disposable matcher for one project from
// the three hierarchy levels (wide → namespace → project), in that order so the
// most specific level wins. Missing files contribute no patterns. An empty
// result is a valid matcher that disposes nothing.
func (s *FSGitStorage) effectiveDisposable(namespace, projectName string) (gitignore.Matcher, error) {
	var patterns []gitignore.Pattern
	for _, p := range []string{
		s.disposableWidePath(),
		s.disposableNamespacePath(namespace),
		s.disposableProjectPath(namespace, projectName),
	} {
		ps, err := loadDisposablePatterns(p)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, ps...)
	}
	return gitignore.NewMatcher(patterns), nil
}

// splitDisposablePath splits a slash-separated within-project path into the
// component slice gitignore.Matcher.Match expects.
func splitDisposablePath(path string) []string {
	return strings.Split(filepath.ToSlash(path), "/")
}
