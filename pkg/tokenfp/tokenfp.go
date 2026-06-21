// Package tokenfp computes a non-secret, one-way fingerprint of an opaque token,
// for correlating the SAME token value across two log lines WITHOUT ever logging the
// value itself (B-54). It is the discriminator for the "issued token fails its own
// Lookup" investigation: the fingerprint logged at issuance and again at the auth
// rejection answers whether the SAME value reached validation (store reset/split) or
// a DIFFERENT value arrived (proxy/stale token) — with no token, code, or secret in
// any log line.
//
// The fingerprint is the first 8 hex chars (32 bits) of SHA-256(token): one-way (the
// token cannot be recovered) and far too short to brute-force a 256-bit handle back,
// while still distinguishing two specific tokens in a log. It is a DIAGNOSTIC
// correlator, never an authenticator.
package tokenfp

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintHexLen is the number of hex characters retained (32 bits).
const fingerprintHexLen = 8

// Fingerprint returns a short one-way fingerprint of token for log correlation, or
// "" for an empty token (so a missing-bearer rejection is visibly distinct from a
// present-but-wrong one). The input value is never returned or recoverable.
func Fingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:fingerprintHexLen]
}
