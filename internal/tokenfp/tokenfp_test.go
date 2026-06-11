package tokenfp

import "testing"

func TestFingerprint_StableAndDistinct(t *testing.T) {
	a := Fingerprint("token-AAAA")
	if a != Fingerprint("token-AAAA") {
		t.Error("fingerprint must be stable for the same token")
	}
	if a == Fingerprint("token-BBBB") {
		t.Error("different tokens must (overwhelmingly likely) fingerprint differently")
	}
	if len(a) != fingerprintHexLen {
		t.Errorf("fingerprint len: got %d want %d", len(a), fingerprintHexLen)
	}
}

func TestFingerprint_EmptyIsEmpty(t *testing.T) {
	if Fingerprint("") != "" {
		t.Error("empty token must fingerprint to empty (so missing-bearer is distinct)")
	}
}

func TestFingerprint_NeverContainsTheToken(t *testing.T) {
	// One-way: the fingerprint must not be (or contain) the input value.
	secret := "SUPER-SECRET-TOKEN-VALUE"
	fp := Fingerprint(secret)
	if fp == secret || len(fp) >= len(secret) {
		t.Errorf("fingerprint must be a short one-way digest, not the value: %q", fp)
	}
}
