package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// B-52 §2.3: each discovery/metadata serve is logged with which document was
// served, so a discovery-path failure is visible (not a black box).

func TestDiscoveryLog_PRMServed(t *testing.T) {
	logger, buf := bufLogger()
	h := ProtectedResourceMetadataHandler(DiscoveryConfig{ExternalURL: "https://rs.example", Logger: logger})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth discovery served")
	if line["document"] != "protected_resource_metadata" {
		t.Errorf("document: want protected_resource_metadata, got %v", line["document"])
	}
}

func TestDiscoveryLog_ASServed(t *testing.T) {
	logger, buf := bufLogger()
	h := AuthorizationServerMetadataHandler(DiscoveryConfig{ExternalURL: "https://rs.example", Logger: logger})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth discovery served")
	if line["document"] != "authorization_server_metadata" {
		t.Errorf("document: want authorization_server_metadata, got %v", line["document"])
	}
}
