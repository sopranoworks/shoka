package ui

import (
	"net/http/httptest"
	"testing"
)

// #7 (/ws/ui side, B-28 project RENAME). RENAME_PROJECT is admin-on-the-namespace (wsLevels,
// NOT super-user — looser than MOVE_PROJECT because the project stays in its namespace): an
// admin on the namespace renames; an admin on a DIFFERENT namespace is refused.
func TestWSUI_RenameProject_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "p"); err != nil {
		t.Fatalf("seed foo/p: %v", err)
	}

	// Admin on a DIFFERENT namespace → DENIED.
	srvBar := httptest.NewServer(withScope("namespace:bar:admin", m))
	defer srvBar.Close()
	connBar := dialWS(t, srvBar.URL)
	defer connBar.Close()
	sendWS(t, connBar, MsgRenameProject, RenameProjectPayload{Namespace: "foo", ProjectName: "p", NewProjectName: "q"})
	if ft := firstFrameType(t, connBar); ft != MsgPermissionDenied {
		t.Fatalf("RENAME_PROJECT must be DENIED for an admin on another namespace, got %s", ft)
	}
	if ps, _ := s.ListProjects("foo"); nsListed(ps, "q") {
		t.Fatal("a denied RENAME_PROJECT must not have renamed the project")
	}

	// Admin on the project's namespace → allowed; the project renames.
	srv := httptest.NewServer(withScope("namespace:foo:admin", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()
	sendWS(t, conn, MsgRenameProject, RenameProjectPayload{Namespace: "foo", ProjectName: "p", NewProjectName: "q"})
	if ft := firstFrameType(t, conn); ft == MsgPermissionDenied {
		t.Fatal("admin-on-namespace must be allowed to RENAME_PROJECT")
	}
	if ps, _ := s.ListProjects("foo"); !nsListed(ps, "q") {
		t.Fatalf("admin RENAME_PROJECT did not rename the project: %v", ps)
	}
	if ps, _ := s.ListProjects("foo"); nsListed(ps, "p") {
		t.Fatalf("admin RENAME_PROJECT left the old name in place: %v", ps)
	}
}

// #7 (/ws/ui side, B-28 namespace RENAME). RENAME_NAMESPACE is SUPER-USER only
// (wsSuperUserOps): a namespace-admin is refused; a super-user passes and the namespace
// (with its projects) relabels.
func TestWSUI_RenameNamespace_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("src", "x"); err != nil { // registers the managed namespace
		t.Fatalf("seed src/x: %v", err)
	}

	// A namespace-admin → DENIED (super-user only).
	srv := httptest.NewServer(withScope("namespace:src:admin", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()
	sendWS(t, conn, MsgRenameNamespace, RenameNamespacePayload{Namespace: "src", NewNamespace: "dst"})
	if ft := firstFrameType(t, conn); ft != MsgPermissionDenied {
		t.Fatalf("RENAME_NAMESPACE must be DENIED for a namespace-admin (super-user only), got %s", ft)
	}
	if ps, _ := s.ListProjects("src"); !nsListed(ps, "x") {
		t.Fatal("a denied RENAME_NAMESPACE must not have relabelled the namespace")
	}

	// A super-user passes and the namespace relabels.
	srvSU := httptest.NewServer(withScope("*", m))
	defer srvSU.Close()
	connSU := dialWS(t, srvSU.URL)
	defer connSU.Close()
	sendWS(t, connSU, MsgRenameNamespace, RenameNamespacePayload{Namespace: "src", NewNamespace: "dst"})
	if ft := firstFrameType(t, connSU); ft == MsgPermissionDenied {
		t.Fatal("super-user must be allowed to RENAME_NAMESPACE")
	}
	if ps, _ := s.ListProjects("dst"); !nsListed(ps, "x") {
		t.Fatalf("super-user RENAME_NAMESPACE did not relabel the namespace to dst: %v", ps)
	}
}
