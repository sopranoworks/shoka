package ingest

import (
	"encoding/base64"
	"testing"
)

func TestDecodeContent_Utf8GatedByAllowlist(t *testing.T) {
	// The utf8 (default) path enforces the SAME closed allowlist as base64: a
	// non-allowlisted extension or an extensionless path is rejected with
	// format_rejected, writing nothing (2026-06-22 utf8-ingest-gate fix). The
	// earlier "utf8 accepts any extension" behaviour was the defect.
	for _, enc := range []string{"", "utf8"} {
		for _, path := range []string{"plain.txt", "blob.bin", "Makefile", "LICENSE", "image.png"} {
			_, _, reason, ok := DecodeContent(path, "hello\n", enc)
			if ok {
				t.Fatalf("encoding=%q path=%q: non-allowlisted utf8 write must be rejected", enc, path)
			}
			if reason != "format_rejected" {
				t.Fatalf("encoding=%q path=%q: want reason format_rejected, got %q", enc, path, reason)
			}
		}
		for _, path := range []string{"a.md", "b.markdown", "c.json", "d.yaml", "e.yml", "f.MD", "sub/g.Yaml"} {
			out, _, _, ok := DecodeContent(path, "hello\n", enc)
			if !ok {
				t.Fatalf("encoding=%q path=%q: allowlisted utf8 write must be accepted", enc, path)
			}
			if out != "hello\n" {
				t.Fatalf("encoding=%q path=%q: got %q", enc, path, out)
			}
		}
	}
}

// TestDecodeContent_AllowlistParityAcrossEncodings proves the gate is identical on
// both paths: the same non-allowlisted path is rejected with format_rejected
// whether the write is utf8 or base64, and the same allowlisted path is accepted
// on both.
func TestDecodeContent_AllowlistParityAcrossEncodings(t *testing.T) {
	for _, path := range []string{"plain.txt", "blob.bin", "Makefile", "image.png"} {
		_, _, utf8Reason, utf8OK := DecodeContent(path, "x", "utf8")
		_, _, b64Reason, b64OK := DecodeContent(path, base64.StdEncoding.EncodeToString([]byte("x")), "base64")
		if utf8OK || b64OK || utf8Reason != "format_rejected" || b64Reason != "format_rejected" {
			t.Fatalf("path %q: want both encodings rejected with format_rejected; utf8 ok=%v reason=%q, base64 ok=%v reason=%q",
				path, utf8OK, utf8Reason, b64OK, b64Reason)
		}
	}
	for _, path := range []string{"a.md", "b.json", "c.yaml"} {
		if _, _, _, ok := DecodeContent(path, "x", "utf8"); !ok {
			t.Fatalf("path %q: allowlisted utf8 write must be accepted", path)
		}
		if _, _, _, ok := DecodeContent(path, base64.StdEncoding.EncodeToString([]byte("x")), "base64"); !ok {
			t.Fatalf("path %q: allowlisted base64 write must be accepted", path)
		}
	}
}

func TestDecodeContent_Base64AllowlistedRoundTrips(t *testing.T) {
	for _, path := range []string{"a.md", "b.markdown", "c.json", "d.yaml", "e.yml", "f.MD", "sub/g.Yaml"} {
		raw := []byte{0xff, 0xfe, 0x00, 0x41} // non-UTF-8, must survive
		out, _, _, ok := DecodeContent(path, base64.StdEncoding.EncodeToString(raw), "base64")
		if !ok {
			t.Fatalf("path %q: allowlisted base64 must be accepted", path)
		}
		if out != string(raw) {
			t.Fatalf("path %q: byte round-trip mismatch", path)
		}
	}
}

func TestDecodeContent_Base64DisallowedRejected(t *testing.T) {
	for _, path := range []string{"x.png", "y.txt", "Makefile", "LICENSE", "z.tar.gz"} {
		_, _, reason, ok := DecodeContent(path, base64.StdEncoding.EncodeToString([]byte("x")), "base64")
		if ok {
			t.Fatalf("path %q: disallowed extension must be rejected on the base64 path", path)
		}
		if reason != "format_rejected" {
			t.Fatalf("path %q: want reason format_rejected, got %q", path, reason)
		}
	}
}

func TestDecodeContent_Base64MalformedAndUnknownEncoding(t *testing.T) {
	if _, _, reason, ok := DecodeContent("x.md", "not base64!!!", "base64"); ok || reason != "invalid_encoding" {
		t.Fatalf("malformed base64: want !ok + invalid_encoding, got ok=%v reason=%q", ok, reason)
	}
	if _, _, reason, ok := DecodeContent("x.md", "abc", "hex"); ok || reason != "invalid_encoding" {
		t.Fatalf("unknown encoding: want !ok + invalid_encoding, got ok=%v reason=%q", ok, reason)
	}
}
