package uiws

// Self-contained test helpers for the pkg/uiws holder proof. They mirror the small
// helpers the internal/ui tests use (withScope/dialWS/sendWS/readUntil/firstFrameType/
// testUserStore/fakeOAuthStore), reproduced here so the holder test stands alone in
// pkg/uiws with no dependency on internal/ui's test files. Deliberately NO
// internal/storage import — the holder proof must compile without a document store.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// withScope wraps a handler so the upgrade request carries a session principal of the
// given scope — the seam authapi.Middleware fills in production (stage 1).
func withScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.Principal{Name: "u", Email: "u@example.com", Scope: scope}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// dialWS opens a ws connection to a test server URL.
func dialWS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// sendWS marshals and writes one {type,payload} frame.
func sendWS(t *testing.T, conn *websocket.Conn, msgType MessageType, payload interface{}) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	data, err := json.Marshal(WSMessage{Type: msgType, Payload: raw})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

// readUntil reads frames until one of msgType arrives, decoding it into dst.
func readUntil(t *testing.T, conn *websocket.Conn, msgType MessageType, dst interface{}, within time.Duration) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("did not receive %s: %v", msgType, err)
		}
		if msg.Type != msgType {
			continue
		}
		if dst != nil {
			if err := json.Unmarshal(msg.Payload, dst); err != nil {
				t.Fatalf("decode %s: %v", msgType, err)
			}
		}
		return
	}
}

// firstFrameType reads the next frame and returns its type.
func firstFrameType(t *testing.T, conn *websocket.Conn) MessageType {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg WSMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return msg.Type
}

// testUserStore opens an isolated userstore for a test.
func testUserStore(t *testing.T) *userstore.Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := userstore.Open(filepath.Join(t.TempDir(), "users.db"), key)
	if err != nil {
		t.Fatalf("open userstore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeOAuthStore is a minimal in-memory OAuthConnectionStore for the holder proof. The
// domain/confidential methods are stubs (the holder test exercises only OAUTH/DOMAIN
// list ops); it satisfies the full interface.
type fakeOAuthStore struct {
	mu      sync.Mutex
	series  []oauthstore.SeriesInfo
	revoked []string
	listErr error
}

func (f *fakeOAuthStore) List() ([]oauthstore.SeriesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]oauthstore.SeriesInfo, len(f.series))
	copy(out, f.series)
	return out, nil
}

func (f *fakeOAuthStore) Revoke(seriesID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, seriesID)
	kept := f.series[:0:0]
	for _, s := range f.series {
		if s.SeriesID != seriesID {
			kept = append(kept, s)
		}
	}
	f.series = kept
	return nil
}

func (f *fakeOAuthStore) ListRegistrations() ([]oauthstore.RegistrationEntry, error) { return nil, nil }
func (f *fakeOAuthStore) CreateRegistration(string, string, time.Time) (oauthstore.RegistrationEntry, error) {
	return oauthstore.RegistrationEntry{}, nil
}
func (f *fakeOAuthStore) GetRegistration(string) (oauthstore.RegistrationEntry, error) {
	return oauthstore.RegistrationEntry{}, oauthstore.ErrNotFound
}
func (f *fakeOAuthStore) UpdateRegistration(oauthstore.RegistrationEntry) error { return nil }
func (f *fakeOAuthStore) DeleteRegistration(string) error                       { return nil }
func (f *fakeOAuthStore) RevokeByDomain(string) (int, error)                    { return 0, nil }
func (f *fakeOAuthStore) GenerateDomainConsent(string) (string, error)          { return "", nil }
func (f *fakeOAuthStore) DomainEntryForClient(string) (oauthstore.RegistrationEntry, bool) {
	return oauthstore.RegistrationEntry{}, false
}
func (f *fakeOAuthStore) IssueConfidentialClient(string, time.Duration, time.Time) (oauthstore.RegistrationEntry, string, error) {
	return oauthstore.RegistrationEntry{}, "", nil
}
func (f *fakeOAuthStore) RevokeByClientID(string) (int, error) { return 0, nil }

func (f *fakeOAuthStore) RevokeByPrincipalEmail(email string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := strings.ToLower(strings.TrimSpace(email))
	kept := f.series[:0:0]
	n := 0
	for _, s := range f.series {
		if strings.EqualFold(strings.TrimSpace(s.Principal.Email), target) {
			f.revoked = append(f.revoked, s.SeriesID)
			n++
			continue
		}
		kept = append(kept, s)
	}
	f.series = kept
	return n, nil
}
