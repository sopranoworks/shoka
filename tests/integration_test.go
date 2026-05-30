package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
)

func TestMCPTools_Integration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-tools-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Test CreateProject tool
	t.Run("CreateProjectTool", func(t *testing.T) {
		handler := tools.CreateProjectHandler(s)
		input := tools.CreateProjectInput{
			Namespace:   "tool-ns",
			ProjectName: "tool-project",
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}
		if output.Message == "" {
			t.Error("expected success message, got empty")
		}

		// Verify project exists
		projectPath := filepath.Join(tmpDir, "tool-ns", "tool-project")
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			t.Errorf("project directory not created: %s", projectPath)
		}
	})

	// Test WriteFile tool
	t.Run("WriteFileTool", func(t *testing.T) {
		handler := tools.WriteFileHandler(s)
		input := tools.WriteFileInput{
			Namespace:   "tool-ns",
			ProjectName: "tool-project",
			Path:        "test.txt",
			Content:     "hello from tool",
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}
		if output.Message == "" {
			t.Error("expected success message, got empty")
		}

		// Verify file content
		content, err := s.ReadFile("tool-ns", "tool-project", "test.txt")
		if err != nil {
			t.Fatalf("failed to read file: %v", err)
		}
		if content != "hello from tool" {
			t.Errorf("expected 'hello from tool', got %q", content)
		}
	})

	// Test ReadFile tool
	t.Run("ReadFileTool", func(t *testing.T) {
		handler := tools.ReadFileHandler(s)
		input := tools.ReadFileInput{
			Namespace:   "tool-ns",
			ProjectName: "tool-project",
			Path:        "test.txt",
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}
		if output.Content != "hello from tool" {
			t.Errorf("expected 'hello from tool', got %q", output.Content)
		}
	})

	// Test ListProjects tool
	t.Run("ListProjectsTool", func(t *testing.T) {
		handler := tools.ListProjectsHandler(s)
		input := tools.ListProjectsInput{
			Namespace: "tool-ns",
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}
		// list_projects returns the prefixed "<ns>/<name>" shape in both the
		// scoped and unscoped cases (B-22 / B-13 namespace surface).
		if len(output.Projects) != 1 || output.Projects[0] != "tool-ns/tool-project" {
			t.Errorf("expected projects ['tool-ns/tool-project'], got %v", output.Projects)
		}
	})
}

func TestFSGitStorage_Integration(t *testing.T) {
	// 1. Initialize a FSGitStorage in a temporary directory.
	tmpDir, err := os.MkdirTemp("", "shoka-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer s.Close()

	namespace := "test-ns"
	projectName := "test-project"

	// 2. Test create_project: Verify directory creation and .git presence.
	t.Run("CreateProject", func(t *testing.T) {
		err := s.CreateProject(namespace, projectName)
		if err != nil {
			t.Fatalf("CreateProject failed: %v", err)
		}

		projectPath := filepath.Join(tmpDir, namespace, projectName)
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			t.Errorf("project directory not created: %s", projectPath)
		}

		gitPath := filepath.Join(projectPath, ".git")
		if _, err := os.Stat(gitPath); os.IsNotExist(err) {
			t.Errorf(".git directory not created: %s", gitPath)
		}
	})

	// 3. Test write_file: Verify file content and that a Git commit is created.
	t.Run("WriteFile", func(t *testing.T) {
		filePath := "docs/README.md"
		content := "# Test Project"
		err := s.WriteFile(namespace, projectName, filePath, content)
		if err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		projectPath := filepath.Join(tmpDir, namespace, projectName)
		fullPath := filepath.Join(projectPath, filePath)
		gotContent, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatalf("failed to read written file: %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("expected content %q, got %q", content, string(gotContent))
		}

		// Commits are asynchronous; wait for the background worker to commit.
		if !s.WaitForWAL(10 * time.Second) {
			t.Fatal("WAL did not drain")
		}

		// Verify Git commit
		r, err := git.PlainOpen(projectPath)
		if err != nil {
			t.Fatalf("failed to open git repo: %v", err)
		}
		ref, err := r.Head()
		if err != nil {
			t.Fatalf("failed to get HEAD: %v", err)
		}
		commit, err := r.CommitObject(ref.Hash())
		if err != nil {
			t.Fatalf("failed to get commit object: %v", err)
		}
		if commit.Message != "Update docs/README.md" {
			t.Errorf("expected commit message %q, got %q", "Update docs/README.md", commit.Message)
		}
	})

	// 4. Test read_file: Verify content retrieval.
	t.Run("ReadFile", func(t *testing.T) {
		filePath := "docs/README.md"
		expectedContent := "# Test Project"
		content, err := s.ReadFile(namespace, projectName, filePath)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if content != expectedContent {
			t.Errorf("expected content %q, got %q", expectedContent, content)
		}
	})

	// 5. Test list_projects: Verify correct listing within a namespace.
	t.Run("ListProjects", func(t *testing.T) {
		projects, err := s.ListProjects(namespace)
		if err != nil {
			t.Fatalf("ListProjects failed: %v", err)
		}
		found := false
		for _, p := range projects {
			if p == projectName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected project %s not found in %v", projectName, projects)
		}

		// Add another project
		err = s.CreateProject(namespace, "another-project")
		if err != nil {
			t.Fatalf("CreateProject failed: %v", err)
		}
		projects, err = s.ListProjects(namespace)
		if err != nil {
			t.Fatalf("ListProjects failed: %v", err)
		}
		if len(projects) < 2 {
			t.Errorf("expected at least 2 projects, got %d", len(projects))
		}
	})

	// 6. Security Verification: Test that invalid namespace or project_name are rejected.
	t.Run("Security_PathTraversal", func(t *testing.T) {
		invalidNames := []string{"..", "sub/dir", "project!", " "}
		for _, name := range invalidNames {
			if err := s.CreateProject("default", name); err == nil {
				t.Errorf("expected error for invalid project name %q, got nil", name)
			}
			if err := s.CreateProject(name, "project"); err == nil {
				t.Errorf("expected error for invalid namespace %q, got nil", name)
			}
		}

		// Empty project name should fail
		if err := s.CreateProject("default", ""); err == nil {
			t.Error("expected error for empty project name, got nil")
		}

		// Empty namespace should succeed (defaults to "default")
		if err := s.CreateProject("", "project-with-empty-ns"); err != nil {
			t.Errorf("expected success for empty namespace, got error: %v", err)
		}

		// Test path traversal in WriteFile/ReadFile
		if err := s.WriteFile(namespace, projectName, "../outside.txt", "content"); err == nil {
			t.Error("expected error for path traversal in WriteFile, got nil")
		}
		if _, err := s.ReadFile(namespace, projectName, "../outside.txt"); err == nil {
			t.Error("expected error for path traversal in ReadFile, got nil")
		}
	})

	// 7. Constraint Verification: Assert that NO .gitignore files are created.
	t.Run("NoGitignore", func(t *testing.T) {
		err := filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Name() == ".gitignore" {
				return fmt.Errorf("found .gitignore at %s", path)
			}
			return nil
		})
		if err != nil {
			t.Errorf("Constraint violation: %v", err)
		}
	})
}
