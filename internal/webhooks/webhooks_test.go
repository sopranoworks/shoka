package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmit_DeliversToSubscribedHook(t *testing.T) {
	type received struct {
		body []byte
		sig  string
	}
	ch := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- received{body: b, sig: r.Header.Get("X-Shoka-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New([]Config{{Name: "t", URL: srv.URL, Events: []string{"file_written"}}})
	n.Emit(Event{Event: "file_written", Namespace: "ns", Project: "p", Path: "a.md", CommitHash: "abc", Timestamp: time.Now()})
	n.Wait()

	select {
	case got := <-ch:
		var ev Event
		if err := json.Unmarshal(got.body, &ev); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if ev.Event != "file_written" || ev.Namespace != "ns" || ev.Project != "p" || ev.Path != "a.md" || ev.CommitHash != "abc" {
			t.Fatalf("unexpected payload: %+v", ev)
		}
		if got.sig != "" {
			t.Fatalf("expected no signature without secret, got %q", got.sig)
		}
	default:
		t.Fatal("webhook was not delivered")
	}
}

func TestEmit_Signature(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-Shoka-Signature")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret := "s3cr3t"
	n := New([]Config{{URL: srv.URL, Events: []string{"file_written"}, Secret: secret}})
	n.Emit(Event{Event: "file_written", Namespace: "ns", Project: "p", Timestamp: time.Now()})
	n.Wait()

	mu.Lock()
	defer mu.Unlock()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
}

func TestEmit_OnlySubscribedEvents(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New([]Config{{URL: srv.URL, Events: []string{"file_deleted"}}})
	n.Emit(Event{Event: "file_written", Namespace: "ns", Project: "p", Timestamp: time.Now()})
	n.Wait()

	if c := atomic.LoadInt32(&count); c != 0 {
		t.Fatalf("expected no delivery for unsubscribed event, got %d", c)
	}
}

func TestEmit_FailureDoesNotBlockOrPanic(t *testing.T) {
	// Connection-refused URL; delivery must fail quietly without blocking Wait.
	n := New([]Config{{URL: "http://127.0.0.1:1/nope", Events: []string{"file_written"}}})
	done := make(chan struct{})
	go func() {
		n.Emit(Event{Event: "file_written", Namespace: "ns", Project: "p", Timestamp: time.Now()})
		n.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Emit/Wait blocked on a failing webhook")
	}
}
