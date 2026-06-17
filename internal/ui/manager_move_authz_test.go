package ui

import (
	"net/http/httptest"
	"testing"
)

// #6 (/ws/ui side, B-28 project MOVE). MOVE_PROJECT is SUPER-USER only (wsSuperUserOps):
// a namespace-admin — even on BOTH the source and the target — is refused; a super-user
// passes and the project actually relocates.
func TestWSUI_MoveProject_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("src", "proj"); err != nil {
		t.Fatalf("seed src/proj: %v", err)
	}
	if err := s.CreateProject("dst", "keep"); err != nil { // registers the managed target namespace
		t.Fatalf("seed dst: %v", err)
	}

	// A namespace-admin on BOTH src and dst → still DENIED (super-user only).
	srv := httptest.NewServer(withScope("namespace:src:admin,namespace:dst:admin", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()
	sendWS(t, conn, MsgMoveProject, MoveProjectPayload{Namespace: "src", ProjectName: "proj", NewNamespace: "dst"})
	if ft := firstFrameType(t, conn); ft != MsgPermissionDenied {
		t.Fatalf("MOVE_PROJECT must be DENIED for an admin-on-both (super-user only), got %s", ft)
	}
	// The op did not run.
	if ps, _ := s.ListProjects("dst"); nsListed(ps, "proj") {
		t.Fatal("a denied MOVE_PROJECT must not have relocated the project")
	}

	// A super-user passes and the project relocates.
	srvSU := httptest.NewServer(withScope("*", m))
	defer srvSU.Close()
	connSU := dialWS(t, srvSU.URL)
	defer connSU.Close()
	sendWS(t, connSU, MsgMoveProject, MoveProjectPayload{Namespace: "src", ProjectName: "proj", NewNamespace: "dst"})
	if ft := firstFrameType(t, connSU); ft == MsgPermissionDenied {
		t.Fatal("super-user must be allowed to MOVE_PROJECT")
	}
	if ps, _ := s.ListProjects("dst"); !nsListed(ps, "proj") {
		t.Fatalf("super-user MOVE_PROJECT did not relocate the project to dst: %v", ps)
	}
	if ps, _ := s.ListProjects("src"); nsListed(ps, "proj") {
		t.Fatalf("super-user MOVE_PROJECT left the project at src: %v", ps)
	}
}
