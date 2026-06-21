package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// b64 is the standard base64 encoding of b — the wire form a dropped file's bytes
// take on the external file drag-and-drop ADD path (B-28).
func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// TestWSUI_SaveBase64_AllowlistedWritesByteFaithful: a SAVE_FILE with
// content_encoding="base64" of an allowlisted .md lands byte-faithful through the
// SHARED ingest helper — including genuinely non-UTF-8 bytes a plain utf8 save
// would mangle at the JSON layer. This is the drag-and-drop ADD happy path.
func TestWSUI_SaveBase64_AllowlistedWritesByteFaithful(t *testing.T) {
	conn, s, _, _ := versioningFixture(t)
	raw := []byte{0xff, 0xfe, 0x00, 0xe9, 0x41} // not valid UTF-8
	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"dropped.md","content":"`+b64(raw)+`","content_encoding":"base64"}`)
	if resp.Type != SaveAck {
		t.Fatalf("type = %s, want SAVE_ACK (payload=%s)", resp.Type, resp.Payload)
	}
	got, _, err := s.ReadFileWithETag("ns", "proj", "dropped.md")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != string(raw) {
		t.Fatalf("byte round-trip mismatch: got %x want %x", got, raw)
	}
}

// TestWSUI_SaveBase64_DisallowedFormatRejected: a base64 SAVE_FILE to a path
// outside the markdown/json/yaml allowlist is rejected (ERROR frame) and nothing
// is written — the same closed allowlist write_file enforces, via the one shared
// helper (no duplicate allowlist).
func TestWSUI_SaveBase64_DisallowedFormatRejected(t *testing.T) {
	conn, s, _, _ := versioningFixture(t)
	for _, path := range []string{"image.png", "notes.txt", "Makefile"} {
		resp := roundTrip(t, conn, SaveFile,
			`{"namespace":"ns","projectName":"proj","path":"`+path+`","content":"`+b64([]byte("x"))+`","content_encoding":"base64"}`)
		if resp.Type != uiws.Error {
			t.Fatalf("path %q: type = %s, want ERROR", path, resp.Type)
		}
		if _, _, rerr := s.ReadFileWithETag("ns", "proj", path); rerr == nil {
			t.Fatalf("path %q: rejected ingest must not write the file", path)
		}
	}
}

// TestWSUI_SaveBase64_NoSilentOverwrite is the core no-silent-overwrite guard
// (operator decision ②): a base64 SAVE_FILE whose destination already exists, sent
// with NO if_match, is REFUSED with a CONFLICT carrying the current etag (it does
// NOT clobber) — mirroring move_file. Re-sending with that etag as if_match
// confirms the overwrite and succeeds.
func TestWSUI_SaveBase64_NoSilentOverwrite(t *testing.T) {
	conn, s, _, _ := versioningFixture(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "exists.md", "original\n", nil); err != nil {
		t.Fatal(err)
	}

	// Drop a file with the same name, no if_match → refused, not overwritten.
	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"exists.md","content":"`+b64([]byte("dropped\n"))+`","content_encoding":"base64"}`)
	if resp.Type != MsgConflict {
		t.Fatalf("type = %s, want CONFLICT (no silent overwrite) payload=%s", resp.Type, resp.Payload)
	}
	var c ConflictPayload
	if err := json.Unmarshal(resp.Payload, &c); err != nil {
		t.Fatal(err)
	}
	if c.CurrentETag == "" {
		t.Fatal("CONFLICT must carry the current etag for confirm-then-overwrite")
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "exists.md"); got != "original\n" {
		t.Fatalf("refused drop must leave the original intact, got %q", got)
	}

	// Confirm the overwrite: resend WITH the current etag as if_match.
	resp2 := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"exists.md","content":"`+b64([]byte("dropped\n"))+`","content_encoding":"base64","if_match":"`+c.CurrentETag+`"}`)
	if resp2.Type != SaveAck {
		t.Fatalf("confirmed overwrite type = %s, want SAVE_ACK (payload=%s)", resp2.Type, resp2.Payload)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "exists.md"); got != "dropped\n" {
		t.Fatalf("confirmed overwrite must replace content, got %q", got)
	}
}

// TestWSUI_SaveBase64_NewPathNoConflict: a base64 SAVE_FILE to a FRESH path (no
// collision) creates it directly — the no-silent-overwrite guard only fires on an
// existing destination.
func TestWSUI_SaveBase64_NewPathNoConflict(t *testing.T) {
	conn, s, _, _ := versioningFixture(t)
	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"brand-new.md","content":"`+b64([]byte("# new\n"))+`","content_encoding":"base64"}`)
	if resp.Type != SaveAck {
		t.Fatalf("type = %s, want SAVE_ACK for a fresh path (payload=%s)", resp.Type, resp.Payload)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "brand-new.md"); got != "# new\n" {
		t.Fatalf("got %q", got)
	}
}

// TestWSUI_SaveBase64_RidesLevelWriteGate: the base64 SAVE_FILE add path rides the
// SAME LevelWrite authz gate as a plain save (no new bypass). The gate keys on the
// message type, so base64 changes nothing about authorization: a read-only
// principal is denied; a write principal passes.
func TestWSUI_SaveBase64_RidesLevelWriteGate(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}

	// Read-only principal → denied.
	srvR := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srvR.Close()
	connR := dialWS(t, srvR.URL)
	defer connR.Close()
	sendWS(t, connR, SaveFile, SaveFilePayload{
		Namespace: "foo", ProjectName: "proj", Path: "drop.md",
		Content: b64([]byte("# x\n")), ContentEncoding: "base64",
	})
	readUntil(t, connR, uiws.MsgPermissionDenied, nil, 2*time.Second)

	// Write principal → passes the gate (not a denial).
	srvW := httptest.NewServer(withScope("namespace:foo:rw", m))
	defer srvW.Close()
	connW := dialWS(t, srvW.URL)
	defer connW.Close()
	sendWS(t, connW, SaveFile, SaveFilePayload{
		Namespace: "foo", ProjectName: "proj", Path: "drop.md",
		Content: b64([]byte("# x\n")), ContentEncoding: "base64",
	})
	if ft := firstFrameType(t, connW); ft == uiws.MsgPermissionDenied {
		t.Fatal("write principal must pass the base64 SAVE_FILE gate")
	}
}
