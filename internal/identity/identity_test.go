package identity

import (
	"context"
	"strings"
	"testing"
)

var defaults = Defaults{
	UserName:    "Osamu Takahashi",
	UserEmail:   "forte.nit@gmail.com",
	AgentName:   "shoka-agent",
	AgentWorker: "",
}

func TestResolve_DefaultAgentWhenNoneDeclared(t *testing.T) {
	id := Resolve(context.Background(), defaults)
	if id.UserName != "Osamu Takahashi" || id.UserEmail != "forte.nit@gmail.com" {
		t.Fatalf("user not from defaults: %+v", id)
	}
	if id.AgentName != "shoka-agent" || id.WorkerID != "" {
		t.Fatalf("agent not the default: %+v", id)
	}
}

func TestResolve_DeclaredAgentOverridesDefault(t *testing.T) {
	ctx := WithAgent(context.Background(), Agent{Name: "claude-code", Worker: "w-42"})
	id := Resolve(ctx, defaults)
	if id.AgentName != "claude-code" {
		t.Fatalf("agent name not overridden: %q", id.AgentName)
	}
	if id.WorkerID != "w-42" {
		t.Fatalf("worker not threaded: %q", id.WorkerID)
	}
	// User remains the configured single-user.
	if id.UserName != "Osamu Takahashi" {
		t.Fatalf("user changed unexpectedly: %q", id.UserName)
	}
}

func TestResolve_PartialDeclarationKeepsDefaults(t *testing.T) {
	ctx := WithAgent(context.Background(), Agent{Name: "", Worker: "w-9"})
	id := Resolve(ctx, defaults)
	if id.AgentName != "shoka-agent" {
		t.Fatalf("empty declared name should keep default: %q", id.AgentName)
	}
	if id.WorkerID != "w-9" {
		t.Fatalf("worker should thread even with empty name: %q", id.WorkerID)
	}
}

func TestResolve_WithUserEmptyMakesConfiguredUserAuthor(t *testing.T) {
	// The web /ws/ui SAVE_FILE path: empty User in single-user mode. The owning
	// user becomes the Author (AuthorIsUser), falling back to the configured user.
	ctx := WithUser(context.Background(), User{})
	id := Resolve(ctx, defaults)
	if !id.AuthorIsUser {
		t.Fatalf("AuthorIsUser should be set when WithUser present: %+v", id)
	}
	if id.UserName != "Osamu Takahashi" || id.UserEmail != "forte.nit@gmail.com" {
		t.Fatalf("user not from defaults: %+v", id)
	}
	if id.AgentName != "shoka-agent" {
		t.Fatalf("agent still resolves to default for the trailer: %q", id.AgentName)
	}
}

func TestResolve_WithUserPopulatedSubstitutesActor(t *testing.T) {
	// The future-auth seam (feasibility check 1): a populated User substitutes the
	// authenticated actor at this single resolution site, no redesign.
	ctx := WithUser(context.Background(), User{Name: "alice", Email: "alice@example.com"})
	id := Resolve(ctx, defaults)
	if !id.AuthorIsUser {
		t.Fatalf("AuthorIsUser should be set: %+v", id)
	}
	if id.UserName != "alice" || id.UserEmail != "alice@example.com" {
		t.Fatalf("populated user not substituted: %+v", id)
	}
}

func TestResolve_NoUserKeepsAgentAuthor(t *testing.T) {
	// The MCP / default path: no WithUser, so the agent stays the Author.
	id := Resolve(context.Background(), defaults)
	if id.AuthorIsUser {
		t.Fatalf("AuthorIsUser must be false without WithUser: %+v", id)
	}
}

func TestWithDefaults_FillsEmptyFromOlderEntry(t *testing.T) {
	// An empty identity (an older WAL entry) gets an intentional author.
	got := CommitIdentity{}.WithDefaults(defaults)
	if got.AgentName != "shoka-agent" || got.UserName != "Osamu Takahashi" {
		t.Fatalf("defaults not filled: %+v", got)
	}
	if got.WorkerID != "" {
		t.Fatalf("worker should not be defaulted: %q", got.WorkerID)
	}
}

func TestAgentEmail_Sanitizes(t *testing.T) {
	if got := AgentEmail("claude-code"); got != "claude-code@agents.shoka.local" {
		t.Fatalf("unexpected: %q", got)
	}
	if got := AgentEmail("Claude Code 3"); got != "claude-code-3@agents.shoka.local" {
		t.Fatalf("unexpected sanitization: %q", got)
	}
}

func TestTrailers(t *testing.T) {
	// No worker -> no Shoka-Worker line.
	id := CommitIdentity{
		UserName:  "Osamu Takahashi",
		UserEmail: "forte.nit@gmail.com",
		AgentName: "claude-code",
	}
	tr := id.Trailers()
	if !strings.Contains(tr, "Shoka-User: Osamu Takahashi <forte.nit@gmail.com>") {
		t.Fatalf("missing user trailer:\n%s", tr)
	}
	if !strings.Contains(tr, "Shoka-Agent: claude-code") {
		t.Fatalf("missing agent trailer:\n%s", tr)
	}
	if strings.Contains(tr, "Shoka-Worker") {
		t.Fatalf("worker trailer present when empty:\n%s", tr)
	}

	// With worker -> Shoka-Worker line present.
	id.WorkerID = "w-42"
	if !strings.Contains(id.Trailers(), "Shoka-Worker: w-42") {
		t.Fatalf("worker trailer missing when set:\n%s", id.Trailers())
	}
}
