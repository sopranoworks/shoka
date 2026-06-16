package ui

import (
	"net/http/httptest"
	"testing"
)

// #4 (/ws/ui side, B-28 ns/proj management). Project create/delete = admin on the target
// namespace (CREATE_PROJECT raised from write); namespace create/delete = SUPER-USER only
// (via authz.IsSuperUser, NOT the namespace-targeted gate a namespace-admin would satisfy).
func TestWSUI_NamespaceProjectOps_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatalf("seed foo/proj: %v", err)
	}

	// A namespace-admin of foo (NOT a super-user).
	srv := httptest.NewServer(withScope("namespace:foo:admin", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()

	// CREATE_PROJECT on foo → allowed (admin on the target namespace).
	sendWS(t, conn, MsgCreateProject, CreateProjectPayload{Namespace: "foo", ProjectName: "newp"})
	if ft := firstFrameType(t, conn); ft == MsgPermissionDenied {
		t.Fatal("namespace:foo:admin must be allowed to CREATE_PROJECT on foo")
	}
	// DELETE_PROJECT on foo → allowed.
	sendWS(t, conn, MsgDeleteProject, CreateProjectPayload{Namespace: "foo", ProjectName: "proj"})
	if ft := firstFrameType(t, conn); ft == MsgPermissionDenied {
		t.Fatal("namespace:foo:admin must be allowed to DELETE_PROJECT on foo")
	}
	// CREATE_PROJECT on bar (foreign namespace) → denied.
	sendWS(t, conn, MsgCreateProject, CreateProjectPayload{Namespace: "bar", ProjectName: "x"})
	if ft := firstFrameType(t, conn); ft != MsgPermissionDenied {
		t.Fatalf("CREATE_PROJECT on bar must be DENIED for foo-admin, got %s", ft)
	}
	// CREATE_NAMESPACE → denied (super-user only; a namespace-admin must NOT pass).
	sendWS(t, conn, MsgCreateNamespace, NamespacePayload{Namespace: "zed"})
	if ft := firstFrameType(t, conn); ft != MsgPermissionDenied {
		t.Fatalf("CREATE_NAMESPACE must be DENIED for a namespace-admin, got %s", ft)
	}
	// DELETE_NAMESPACE → denied.
	sendWS(t, conn, MsgDeleteNamespace, NamespacePayload{Namespace: "foo"})
	if ft := firstFrameType(t, conn); ft != MsgPermissionDenied {
		t.Fatalf("DELETE_NAMESPACE must be DENIED for a namespace-admin, got %s", ft)
	}

	// A super-user passes the namespace ops.
	srvSU := httptest.NewServer(withScope("*", m))
	defer srvSU.Close()
	connSU := dialWS(t, srvSU.URL)
	defer connSU.Close()

	sendWS(t, connSU, MsgCreateNamespace, NamespacePayload{Namespace: "zed"})
	if ft := firstFrameType(t, connSU); ft == MsgPermissionDenied {
		t.Fatal("super-user must be allowed to CREATE_NAMESPACE")
	}
	// Confirm the namespace really exists now (the op ran, not just the gate).
	if nss, _ := s.ListNamespaces(); !nsListed(nss, "zed") {
		t.Fatalf("super-user CREATE_NAMESPACE did not create zed: %v", nss)
	}
	sendWS(t, connSU, MsgDeleteNamespace, NamespacePayload{Namespace: "zed"})
	if ft := firstFrameType(t, connSU); ft == MsgPermissionDenied {
		t.Fatal("super-user must be allowed to DELETE_NAMESPACE")
	}
}

func nsListed(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
