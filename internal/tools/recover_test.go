package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage"
)

type fakeRecoverer struct {
	state storage.ProjectState
	err   error
	gotNS string
	gotPr string
}

func (f *fakeRecoverer) ResyncToHead(namespace, projectName string) (storage.ProjectState, error) {
	f.gotNS, f.gotPr = namespace, projectName
	return f.state, f.err
}

func TestRecoverProjectHandler_HealthyRecovers(t *testing.T) {
	f := &fakeRecoverer{state: storage.StateHealthy}
	_, out, err := RecoverProjectHandler(f)(context.Background(), nil, RecoverProjectInput{Namespace: "ns", ProjectName: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Recovered || out.State != "healthy" {
		t.Fatalf("recovered=%v state=%q, want true/healthy", out.Recovered, out.State)
	}
	if f.gotNS != "ns" || f.gotPr != "proj" {
		t.Fatalf("recoverer got %s/%s, want ns/proj", f.gotNS, f.gotPr)
	}
}

func TestRecoverProjectHandler_DefaultsNamespace(t *testing.T) {
	f := &fakeRecoverer{state: storage.StateHealthy}
	if _, _, err := RecoverProjectHandler(f)(context.Background(), nil, RecoverProjectInput{ProjectName: "proj"}); err != nil {
		t.Fatal(err)
	}
	if f.gotNS != "default" {
		t.Fatalf("namespace defaulted to %q, want default", f.gotNS)
	}
}

func TestRecoverProjectHandler_GenuineDriftStaysCorrupted(t *testing.T) {
	f := &fakeRecoverer{state: storage.StateCorrupted}
	_, out, err := RecoverProjectHandler(f)(context.Background(), nil, RecoverProjectInput{Namespace: "ns", ProjectName: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Recovered {
		t.Fatal("a genuinely-corrupted project must not report recovered")
	}
	if out.State != "corrupted" || !strings.Contains(out.Message, "accept-head") {
		t.Fatalf("expected corrupted + destructive-mode guidance, got state=%q msg=%q", out.State, out.Message)
	}
}

func TestRecoverProjectHandler_MissingProjectIsError(t *testing.T) {
	f := &fakeRecoverer{state: storage.StateHealthy}
	res, _, err := RecoverProjectHandler(f)(context.Background(), nil, RecoverProjectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing project_name must return an IsError result")
	}
}

func TestRecoverProjectHandler_SurfacesError(t *testing.T) {
	f := &fakeRecoverer{state: storage.StateHealthy, err: errors.New("boom")}
	res, _, err := RecoverProjectHandler(f)(context.Background(), nil, RecoverProjectInput{Namespace: "ns", ProjectName: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("a ResyncToHead error must surface as an IsError result")
	}
}
