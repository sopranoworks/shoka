package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
)

func TestPhase2Tools_Integration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-phase2-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	ctx := context.Background()
	namespace := "phase2-ns"
	projectName := "phase2-project"

	// Setup: Create project
	err = s.CreateProject(namespace, projectName)
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Test ListFiles tool
	t.Run("ListFilesTool", func(t *testing.T) {
		// Create some files
		s.WriteFile(namespace, projectName, "file1.txt", "content1")
		s.WriteFile(namespace, projectName, "dir/file2.txt", "content2")

		handler := tools.ListFilesHandler(s)
		input := tools.ListFilesInput{
			Namespace:   namespace,
			ProjectName: projectName,
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}

		// Note: ListFiles might return files in different order
		foundFile1 := false
		foundDir := false
		for _, f := range output.Files {
			if f == "file1.txt" {
				foundFile1 = true
			}
			if f == "dir/" {
				foundDir = true
			}
		}
		if !foundFile1 || !foundDir {
			t.Errorf("expected file1.txt and dir/, got %v", output.Files)
		}
	})

	// Test DeleteFile tool
	t.Run("DeleteFileTool", func(t *testing.T) {
		handler := tools.DeleteFileHandler(s)
		input := tools.DeleteFileInput{
			Namespace:   namespace,
			ProjectName: projectName,
			Path:        "file1.txt",
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

		// Verify file is gone
		_, err = s.ReadFile(namespace, projectName, "file1.txt")
		if err == nil {
			t.Error("expected error reading deleted file, got nil")
		}

		// Verify Git commit
		projectPath := filepath.Join(tmpDir, namespace, projectName)
		r, _ := git.PlainOpen(projectPath)
		ref, _ := r.Head()
		commit, _ := r.CommitObject(ref.Hash())
		if commit.Message != "Delete file1.txt" {
			t.Errorf("expected commit message 'Delete file1.txt', got %q", commit.Message)
		}
	})

	// Test GetHistory tool
	t.Run("GetHistoryTool", func(t *testing.T) {
		// Create/Update a file multiple times
		s.WriteFile(namespace, projectName, "history.txt", "v1")
		s.WriteFile(namespace, projectName, "history.txt", "v2")

		handler := tools.GetHistoryHandler(s)
		input := tools.GetHistoryInput{
			Namespace:   namespace,
			ProjectName: projectName,
			Path:        "history.txt",
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}

		if len(output.History) != 2 {
			t.Errorf("expected 2 history entries, got %d", len(output.History))
		}
		if output.History[0].Message != "Update history.txt" {
			t.Errorf("expected latest commit message 'Update history.txt', got %q", output.History[0].Message)
		}
	})

	// Test ReadFileAtVersion tool
	t.Run("ReadFileAtVersionTool", func(t *testing.T) {
		// Get history to find v1 hash
		historyHandler := tools.GetHistoryHandler(s)
		_, historyOutput, _ := historyHandler(ctx, nil, tools.GetHistoryInput{
			Namespace:   namespace,
			ProjectName: projectName,
			Path:        "history.txt",
		})

		if len(historyOutput.History) < 2 {
			t.Fatalf("expected at least 2 history entries, got %d", len(historyOutput.History))
		}

		v1Hash := historyOutput.History[1].Hash // history[0] is v2, history[1] is v1

		handler := tools.ReadFileAtVersionHandler(s)
		input := tools.ReadFileAtVersionInput{
			Namespace:   namespace,
			ProjectName: projectName,
			Path:        "history.txt",
			CommitHash:  v1Hash,
		}
		res, output, err := handler(ctx, nil, input)
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("tool returned error: %v", res.Content[0].(*mcp.TextContent).Text)
		}

		if output.Content != "v1" {
			t.Errorf("expected content 'v1', got %q", output.Content)
		}
	})
}
