package ingest

import (
	"encoding/base64"
	"testing"
)

func TestDecodeContent_Utf8PassesThroughAnyExtension(t *testing.T) {
	for _, enc := range []string{"", "utf8"} {
		out, _, _, ok := DecodeContent("plain.txt", "hello\n", enc)
		if !ok {
			t.Fatalf("encoding=%q: utf8 path must accept any extension", enc)
		}
		if out != "hello\n" {
			t.Fatalf("encoding=%q: got %q", enc, out)
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
