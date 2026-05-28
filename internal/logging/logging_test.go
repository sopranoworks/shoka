package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNew_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("info", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Debug("should-be-suppressed")
	lg.Info("should-appear")
	out := buf.String()
	if strings.Contains(out, "should-be-suppressed") {
		t.Errorf("debug line leaked at info level: %q", out)
	}
	if !strings.Contains(out, "should-appear") {
		t.Errorf("info line missing: %q", out)
	}
}

func TestNew_DebugEmitsDebug(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("debug", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Debug("dbg-line")
	if !strings.Contains(buf.String(), "dbg-line") {
		t.Errorf("debug line missing at debug level: %q", buf.String())
	}
}

func TestNew_DefaultsAndFormat(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("", "json", &buf) // "" level -> info
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("jline", "k", "v")
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("expected json output, got %q", out)
	}
}

func TestNew_InvalidValues(t *testing.T) {
	if _, err := New("loud", "text", &bytes.Buffer{}); err == nil {
		t.Error("expected error for invalid level")
	}
	if _, err := New("info", "yaml", &bytes.Buffer{}); err == nil {
		t.Error("expected error for invalid format")
	}
}

// errWriter always fails, modelling a closed log sink.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("sink closed") }

func TestNew_WriterErrorDoesNotPropagate(t *testing.T) {
	lg, err := New("info", "text", errWriter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// slog swallows handler write errors; this must not panic or block.
	lg.Info("this write fails at the sink")
	lg.Error("so does this")
}
