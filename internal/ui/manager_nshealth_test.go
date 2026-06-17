package ui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/storage"
)

func readFrame(t *testing.T, conn *websocket.Conn) WSMessage {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg WSMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return msg
}

func nsHealthNames(t *testing.T, payload json.RawMessage) []string {
	t.Helper()
	var report storage.HealthReport
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatalf("decode health report: %v", err)
	}
	var names []string
	for _, nh := range report.Namespaces {
		names = append(names, nh.Name)
	}
	return names
}

// #6 (/ws/ui) — NAMESPACE_HEALTH is admin-gated and admin-filtered: a namespace-admin sees
// only their namespace; a super-user sees all; a non-admin is denied. NAMESPACE_RECOVER's
// whole-namespace actions are super-user only (the handler tightens beyond admin-on-ns).
func TestWSUI_NamespaceHealth_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "p"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("bar", "q"); err != nil {
		t.Fatal(err)
	}

	// A namespace-admin of foo: sees ONLY foo.
	srvFoo := httptest.NewServer(withScope("namespace:foo:admin", m))
	defer srvFoo.Close()
	connFoo := dialWS(t, srvFoo.URL)
	defer connFoo.Close()
	sendWS(t, connFoo, MsgNamespaceHealth, struct{}{})
	frame := readFrame(t, connFoo)
	if frame.Type == MsgPermissionDenied {
		t.Fatal("a namespace-admin must be allowed to read NAMESPACE_HEALTH")
	}
	if names := nsHealthNames(t, frame.Payload); len(names) != 1 || names[0] != "foo" {
		t.Fatalf("namespace-admin health = %v, want [foo] only", names)
	}

	// A super-user: sees all managed namespaces.
	srvSU := httptest.NewServer(withScope("*", m))
	defer srvSU.Close()
	connSU := dialWS(t, srvSU.URL)
	defer connSU.Close()
	sendWS(t, connSU, MsgNamespaceHealth, struct{}{})
	frame = readFrame(t, connSU)
	if frame.Type == MsgPermissionDenied {
		t.Fatal("a super-user must read NAMESPACE_HEALTH")
	}
	if names := nsHealthNames(t, frame.Payload); len(names) != 2 {
		t.Fatalf("super-user health namespaces = %v, want both foo and bar", names)
	}

	// A non-admin (read-only) is denied.
	srvRO := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srvRO.Close()
	connRO := dialWS(t, srvRO.URL)
	defer connRO.Close()
	sendWS(t, connRO, MsgNamespaceHealth, struct{}{})
	if ft := readFrame(t, connRO).Type; ft != MsgPermissionDenied {
		t.Fatalf("read-only must be DENIED NAMESPACE_HEALTH, got %s", ft)
	}
}

func TestWSUI_NamespaceRecover_Authz(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "p"); err != nil {
		t.Fatal(err)
	}

	// A namespace-admin of foo: a whole-namespace recovery action is super-user only → denied.
	srvFoo := httptest.NewServer(withScope("namespace:foo:admin", m))
	defer srvFoo.Close()
	connFoo := dialWS(t, srvFoo.URL)
	defer connFoo.Close()
	sendWS(t, connFoo, MsgNamespaceRecover, NamespaceRecoverPayload{Action: "drop_missing", Namespace: "foo"})
	if ft := readFrame(t, connFoo).Type; ft != MsgPermissionDenied {
		t.Fatalf("namespace-level recover must be DENIED for a namespace-admin, got %s", ft)
	}
	// But a PROJECT-level recover on its own namespace is allowed (reaches the op).
	sendWS(t, connFoo, MsgNamespaceRecover, NamespaceRecoverPayload{Action: "drop_missing", Namespace: "foo", ProjectName: "ghost"})
	if ft := readFrame(t, connFoo).Type; ft == MsgPermissionDenied {
		t.Fatal("project-level recover on its own namespace must be allowed for the namespace-admin")
	}

	// A namespace-admin of bar acting on foo (project-level) → denied by the gate.
	srvBar := httptest.NewServer(withScope("namespace:bar:admin", m))
	defer srvBar.Close()
	connBar := dialWS(t, srvBar.URL)
	defer connBar.Close()
	sendWS(t, connBar, MsgNamespaceRecover, NamespaceRecoverPayload{Action: "drop_missing", Namespace: "foo", ProjectName: "ghost"})
	if ft := readFrame(t, connBar).Type; ft != MsgPermissionDenied {
		t.Fatalf("admin on bar must be DENIED recover on foo, got %s", ft)
	}

	// A super-user passes the whole-namespace action (reaches the op; not a denial).
	srvSU := httptest.NewServer(withScope("*", m))
	defer srvSU.Close()
	connSU := dialWS(t, srvSU.URL)
	defer connSU.Close()
	sendWS(t, connSU, MsgNamespaceRecover, NamespaceRecoverPayload{Action: "adopt", Namespace: "foo"})
	if ft := readFrame(t, connSU).Type; ft == MsgPermissionDenied {
		t.Fatal("super-user must pass a whole-namespace recovery action")
	}
}
