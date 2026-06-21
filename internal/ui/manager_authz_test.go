package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/pkg/auth"
)

// withScope wraps the manager so the upgrade request carries a session principal of
// the given scope — the seam authapi.Middleware fills in production (stage 1). This
// lets the /ws/ui flip be tested with a real scoped principal.
func withScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.Principal{Name: "u", Email: "u@example.com", Scope: scope}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// firstFrameType reads the next frame and returns its type (a single response; these
// requests produce no NOTIFY to the originator).
func firstFrameType(t *testing.T, conn *websocket.Conn) MessageType {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg WSMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return msg.Type
}

// TestWSUI_AuthzFlip_ReadOnlyScope is the /ws/ui RED→GREEN flip: a session principal
// scoped namespace:foo:r may READ foo, but SAVE/DELETE on foo, any op on bar, and the
// admin RECOVER_PROJECT are refused with PERMISSION_DENIED — through the SAME
// authz.Authorize the MCP middleware uses. (Before the flip the dormant gate passed
// everything.)
func TestWSUI_AuthzFlip_ReadOnlyScope(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatalf("create foo/proj: %v", err)
	}
	if _, err := s.Write(context.Background(), "", "foo", "proj", "f.md", "# F\n", nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srv.Close()

	conn := dialWS(t, srv.URL)
	defer conn.Close()

	// READ foo → gate passes (the response is the read result, NOT a denial).
	sendWS(t, conn, ReadFile, ReadFilePayload{Namespace: "foo", ProjectName: "proj", Path: "f.md"})
	if ft := firstFrameType(t, conn); ft == MsgPermissionDenied {
		t.Fatal("read-only foo must be allowed to READ_FILE on foo")
	}

	// SAVE foo → denied (read-only).
	sendWS(t, conn, SaveFile, SaveFilePayload{Namespace: "foo", ProjectName: "proj", Path: "f.md", Content: "x"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)

	// DELETE foo → denied.
	sendWS(t, conn, MsgDeleteFile, DeleteFilePayload{Namespace: "foo", ProjectName: "proj", Path: "f.md"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)

	// READ bar (foreign namespace) → denied.
	sendWS(t, conn, ReadFile, ReadFilePayload{Namespace: "bar", ProjectName: "proj", Path: "f.md"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)

	// RECOVER_PROJECT foo → denied (admin required, foo:r is not admin).
	sendWS(t, conn, MsgRecoverProject, RecoverProjectPayload{Namespace: "foo", ProjectName: "proj"})
	readUntil(t, conn, MsgPermissionDenied, nil, 2*time.Second)
}

// TestWSUI_AuthzFlip_SuperUserAndNoLockout proves a super-user passes a write, and the
// no-principal connection (the empty-store / single-operator no-lockout path) is
// treated as super-user — neither is denied.
func TestWSUI_AuthzFlip_SuperUserAndNoLockout(t *testing.T) {
	// Super-user principal.
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()
	sendWS(t, conn, SaveFile, SaveFilePayload{Namespace: "foo", ProjectName: "proj", Path: "g.md", Content: "# G\n"})
	if ft := firstFrameType(t, conn); ft == MsgPermissionDenied {
		t.Fatal("super-user must pass SAVE_FILE")
	}

	// No principal at all (no wrapper) → no-lockout super-user pass-through.
	m2, s2, _ := newSharedCenterManager(t)
	if err := s2.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(m2)
	defer srv2.Close()
	conn2 := dialWS(t, srv2.URL)
	defer conn2.Close()
	sendWS(t, conn2, SaveFile, SaveFilePayload{Namespace: "foo", ProjectName: "proj", Path: "h.md", Content: "# H\n"})
	if ft := firstFrameType(t, conn2); ft == MsgPermissionDenied {
		t.Fatal("no-principal (no-lockout) connection must be treated as super-user")
	}
}
