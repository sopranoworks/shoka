package tools

import (
	"context"
	"encoding/base64"
	"os"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage"
)

func newWriteStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-tools-write-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestWriteFileHandler_Base64UTF8Markdown: a UTF-8 markdown file ingested via
// base64 lands byte-faithful (the common case, incl. CJK).
func TestWriteFileHandler_Base64UTF8Markdown(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	original := "# 見出し\n\n本文の段落です。\n"
	enc := base64.StdEncoding.EncodeToString([]byte(original))
	res, out, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "notes.md",
		Content: enc, ContentEncoding: "base64",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected a new etag")
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "notes.md")
	if got != original {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, original)
	}
}

// TestWriteFileHandler_Base64NonUTF8Survives is the B-46a failure case, now
// closed: genuinely non-UTF-8 bytes survive intact via base64, where a plain
// (utf8) write would have silently mangled them to U+FFFD at the JSON layer.
func TestWriteFileHandler_Base64NonUTF8Survives(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	// 0xff 0xfe 0x00 0xe9 — not valid UTF-8 (a latin-1 byte, a stray 0xff/0xfe,
	// and a NUL for good measure). A .md path keeps it on the allowed ingest list.
	rawBytes := []byte{0xff, 0xfe, 0x00, 0xe9, 0x41}
	enc := base64.StdEncoding.EncodeToString(rawBytes)
	res, out, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "raw.md",
		Content: enc, ContentEncoding: "base64",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected a new etag")
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "raw.md")
	if got != string(rawBytes) {
		t.Fatalf("non-UTF-8 round-trip mismatch:\n got %x\nwant %x", got, rawBytes)
	}
}

// TestWriteFileHandler_Base64DisallowedFormatRejected: a base64 ingest to a path
// outside the allowlist is rejected server-side with a typed reason, and nothing
// is written.
func TestWriteFileHandler_Base64DisallowedFormatRejected(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	enc := base64.StdEncoding.EncodeToString([]byte("whatever"))
	for _, path := range []string{"doc.pdf", "image.png", "Makefile", "LICENSE", "archive.tar.gz"} {
		res, out, err := h(context.Background(), nil, WriteFileInput{
			Namespace: "ns", ProjectName: "proj", Path: path,
			Content: enc, ContentEncoding: "base64",
		})
		if err != nil {
			t.Fatalf("handler err for %q: %v", path, err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("path %q: disallowed format must be an error result", path)
		}
		if out.Reason != "format_rejected" {
			t.Fatalf("path %q: want reason format_rejected, got %q", path, out.Reason)
		}
		// Nothing was written.
		if _, _, rerr := s.ReadFileWithETag("ns", "proj", path); rerr == nil {
			t.Fatalf("path %q: rejected ingest must not write the file", path)
		}
	}
}

// TestWriteFileHandler_Base64AllowedExtensionsCaseInsensitive: the allowed set is
// accepted, case-insensitively.
func TestWriteFileHandler_Base64AllowedExtensionsCaseInsensitive(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	enc := base64.StdEncoding.EncodeToString([]byte("ok"))
	for _, path := range []string{"a.md", "b.markdown", "c.json", "d.yaml", "e.yml", "f.MD", "g.JSON", "sub/dir/h.Yaml"} {
		res, _, err := h(context.Background(), nil, WriteFileInput{
			Namespace: "ns", ProjectName: "proj", Path: path,
			Content: enc, ContentEncoding: "base64",
		})
		if err != nil {
			t.Fatalf("handler err for %q: %v", path, err)
		}
		if res != nil && res.IsError {
			t.Fatalf("path %q: allowed format must succeed, got error %+v", path, res)
		}
	}
}

// TestWriteFileHandler_Base64InvalidContentRejected: malformed base64 is a typed
// error and writes nothing.
func TestWriteFileHandler_Base64InvalidContentRejected(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	res, out, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "x.md",
		Content: "not valid base64!!!", ContentEncoding: "base64",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("invalid base64 must be an error result")
	}
	if out.Reason != "invalid_encoding" {
		t.Fatalf("want reason invalid_encoding, got %q", out.Reason)
	}
	if _, _, rerr := s.ReadFileWithETag("ns", "proj", "x.md"); rerr == nil {
		t.Fatal("invalid base64 must not write the file")
	}
}

// TestWriteFileHandler_UnsupportedEncodingRejected: an unknown content_encoding
// is a typed error.
func TestWriteFileHandler_UnsupportedEncodingRejected(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	res, out, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "x.md",
		Content: "abc", ContentEncoding: "hex",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("unsupported content_encoding must be an error result")
	}
	if out.Reason != "invalid_encoding" {
		t.Fatalf("want reason invalid_encoding, got %q", out.Reason)
	}
}

// TestWriteFileHandler_PlainWriteGatedByAllowlist: a plain (utf8) write is gated by
// the SAME allowlist as base64 — a .txt plain write is rejected with
// format_rejected and nothing is written (2026-06-22 utf8-ingest-gate fix; the
// earlier "plain write unaffected" behaviour was the defect). An allowlisted .md
// plain write still succeeds.
func TestWriteFileHandler_PlainWriteGatedByAllowlist(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	for _, enc := range []string{"", "utf8"} {
		res, out, err := h(context.Background(), nil, WriteFileInput{
			Namespace: "ns", ProjectName: "proj", Path: "plain.txt",
			Content: "hello\n", ContentEncoding: enc,
		})
		if err != nil {
			t.Fatalf("handler err (encoding=%q): %v", enc, err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("encoding=%q: plain .txt write must be rejected", enc)
		}
		if out.Reason != "format_rejected" {
			t.Fatalf("encoding=%q: want reason format_rejected, got %q", enc, out.Reason)
		}
	}
	// An allowlisted plain (utf8) write still succeeds and is stored verbatim.
	res, _, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "doc.md", Content: "hello\n",
	})
	if err != nil || (res != nil && res.IsError) {
		t.Fatalf("allowlisted .md plain write must succeed, got err=%v res=%+v", err, res)
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "doc.md")
	if got != "hello\n" {
		t.Fatalf("got %q, want %q", got, "hello\n")
	}
}

// TestWriteFileHandler_Base64CreatesParentDirs confirms §3.5 layer 3: ingesting
// to a multi-segment path whose intermediate directory does not yet exist
// succeeds (writeTransformed MkdirAll's the parent) — no directory-creation
// command needed.
func TestWriteFileHandler_Base64CreatesParentDirs(t *testing.T) {
	s := newWriteStorage(t)
	h := WriteFileHandler(s)
	enc := base64.StdEncoding.EncodeToString([]byte("deep\n"))
	res, _, err := h(context.Background(), nil, WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "docs/notes/new.md",
		Content: enc, ContentEncoding: "base64",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("ingest into a new directory hierarchy must succeed, got %+v", res)
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "docs/notes/new.md")
	if got != "deep\n" {
		t.Fatalf("got %q, want %q", got, "deep\n")
	}
}
