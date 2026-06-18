package ui

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWSUI_DeletedLog_AuthzAdminOnly: LIST_DELETED and REVIVE_FILE are admin on the
// target namespace. A read-only principal is refused with PERMISSION_DENIED; an
// admin on the namespace passes the gate (the response is not a denial).
func TestWSUI_DeletedLog_AuthzAdminOnly(t *testing.T) {
	// Read-only on foo → both ops denied.
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "foo", "proj", "f.md", "# F\n", nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()

	sendWS(t, conn, MsgListDeleted, ListDeletedRequest{Namespace: "foo", ProjectName: "proj"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)
	sendWS(t, conn, MsgReviveFile, ReviveFileRequest{Namespace: "foo", ProjectName: "proj", Path: "f.md"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)

	// Admin on foo → LIST_DELETED passes the gate (a real response, not a denial).
	m2, s2, _ := newSharedCenterManager(t)
	if err := s2.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(withScope("namespace:foo:admin", m2))
	defer srv2.Close()
	conn2 := dialWS(t, srv2.URL)
	defer conn2.Close()
	sendWS(t, conn2, MsgListDeleted, ListDeletedRequest{Namespace: "foo", ProjectName: "proj"})
	if ft := firstFrameType(t, conn2); ft == MsgPermissionDenied {
		t.Fatal("admin on foo must be allowed to LIST_DELETED on foo")
	}
}
