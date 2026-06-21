package libstatus

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func TestNew_States(t *testing.T) {
	// Unconfigured: no provider/model.
	u := New(llm.LLMConfig{})
	if s := u.Get(); s.Configured || s.Kind != "unconfigured" {
		t.Errorf("unconfigured snapshot = %+v, want {Configured:false, Kind:unconfigured}", s)
	}
	// Configured but not yet checked.
	c := New(llm.LLMConfig{Provider: "anthropic", Model: "claude-x"})
	if s := c.Get(); !s.Configured || s.Kind != "unknown" || s.Provider != "anthropic" || s.Model != "claude-x" {
		t.Errorf("configured snapshot = %+v, want {Configured:true, Kind:unknown, provider/model set}", s)
	}
}

func TestRefresh_UnconfiguredIsNoNetwork(t *testing.T) {
	// Refresh on an unconfigured checker must NOT make a call; it returns the
	// cached unconfigured snapshot.
	u := New(llm.LLMConfig{})
	got := u.Refresh(context.Background())
	if got.Configured || got.Kind != "unconfigured" {
		t.Errorf("Refresh(unconfigured) = %+v, want unconfigured", got)
	}
}
