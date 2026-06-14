package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
)

func bufLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestLoggedTool_ReceivedAndCompleted_OK(t *testing.T) {
	lg, buf := bufLogger(t)
	h := func(_ context.Context, _ *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
		return nil, WriteFileOutput{Message: "ok"}, nil
	}
	wrapped := LoggedTool(lg, "write_file", h)
	_, _, err := wrapped(context.Background(), nil, WriteFileInput{ProjectName: "p", Path: "a.md", Content: "SECRET-CONTENT"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "tool call received") || !strings.Contains(out, "tool call completed") {
		t.Errorf("missing lifecycle lines: %q", out)
	}
	if !strings.Contains(out, "write_file") || !strings.Contains(out, "project=p") || !strings.Contains(out, "path=a.md") {
		t.Errorf("missing metadata: %q", out)
	}
	if !strings.Contains(out, "outcome=ok") {
		t.Errorf("missing ok outcome: %q", out)
	}
}

func TestLoggedTool_NeverLogsContent(t *testing.T) {
	lg, buf := bufLogger(t)
	h := func(_ context.Context, _ *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
		return nil, WriteFileOutput{Message: "ok"}, nil
	}
	wrapped := LoggedTool(lg, "write_file", h)
	const secret = "TOP-SECRET-FILE-BODY-9f3a"
	_, _, _ = wrapped(context.Background(), nil, WriteFileInput{ProjectName: "p", Path: "a.md", Content: secret})
	if strings.Contains(buf.String(), secret) {
		t.Errorf("content leaked into logs: %q", buf.String())
	}
}

func TestLoggedTool_ClassifiesErrors(t *testing.T) {
	cases := []struct {
		name, text, wantClass string
	}{
		{"validation", "project_name and path are required", "validation_error"},
		{"invalid", "invalid namespace or project_name", "validation_error"},
		{"conflict", "version conflict: file is now at abc", "version_conflict"},
		{"notfound", "failed to read file: project not found", "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lg, buf := bufLogger(t)
			h := func(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: c.text}}}, ReadFileOutput{}, nil
			}
			wrapped := LoggedTool(lg, "read_file", h)
			_, _, _ = wrapped(context.Background(), nil, ReadFileInput{ProjectName: "p", Path: "a.md"})
			out := buf.String()
			if !strings.Contains(out, "outcome=error") || !strings.Contains(out, "error_class="+c.wantClass) {
				t.Errorf("want error_class=%s, got: %q", c.wantClass, out)
			}
		})
	}
}

func TestLoggedTool_RecoversPanic(t *testing.T) {
	lg, buf := bufLogger(t)
	h := func(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
		panic("boom")
	}
	wrapped := LoggedTool(lg, "read_file", h)
	res, _, err := wrapped(context.Background(), nil, ReadFileInput{ProjectName: "p", Path: "a.md"})
	if err != nil {
		t.Fatalf("panic must be recovered to nil Go error, got %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result after panic, got %+v", res)
	}
	out := buf.String()
	if !strings.Contains(out, "tool handler panicked") || !strings.Contains(out, "outcome=panic") {
		t.Errorf("missing panic logging: %q", out)
	}
}

func TestLoggedTool_NilLoggerSafe(t *testing.T) {
	h := func(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
		return nil, ReadFileOutput{Content: "x"}, nil
	}
	wrapped := LoggedTool(nil, "read_file", h) // must not panic
	if _, _, err := wrapped(context.Background(), nil, ReadFileInput{ProjectName: "p", Path: "a.md"}); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// A non-nil Go error (the unexpected-failure path) is classified and surfaced at
// ERROR via "tool handler returned an error", while the completed line still
// records outcome=error with the class.
func TestLoggedTool_GoErrorPath(t *testing.T) {
	t.Run("plain error -> internal", func(t *testing.T) {
		lg, buf := bufLogger(t)
		h := func(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
			return nil, ReadFileOutput{}, fmt.Errorf("something broke")
		}
		wrapped := LoggedTool(lg, "read_file", h)
		_, _, err := wrapped(context.Background(), nil, ReadFileInput{ProjectName: "p", Path: "a.md"})
		if err == nil {
			t.Fatal("expected the Go error to be propagated unchanged")
		}
		out := buf.String()
		if !strings.Contains(out, "tool handler returned an error") {
			t.Errorf("missing ERROR event for Go error: %q", out)
		}
		if !strings.Contains(out, "outcome=error") || !strings.Contains(out, "error_class=internal") {
			t.Errorf("want outcome=error class=internal: %q", out)
		}
	})

	t.Run("VersionConflictError -> version_conflict", func(t *testing.T) {
		lg, buf := bufLogger(t)
		h := func(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
			return nil, ReadFileOutput{}, &storage.VersionConflictError{Expected: "aaa", Current: "bbb"}
		}
		wrapped := LoggedTool(lg, "read_file", h)
		_, _, err := wrapped(context.Background(), nil, ReadFileInput{ProjectName: "p", Path: "a.md"})
		if err == nil {
			t.Fatal("expected the Go error to be propagated unchanged")
		}
		if !strings.Contains(buf.String(), "error_class=version_conflict") {
			t.Errorf("want error_class=version_conflict via errors.As: %q", buf.String())
		}
	})
}
