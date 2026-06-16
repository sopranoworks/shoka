package userstore

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters — the OWASP 2024+ / RFC 9106 baseline for interactive
// logins. They are named constants so a deployment can be raised later without a
// schema change: every hash is stored as a self-describing PHC string carrying the
// exact params used, so an old hash still verifies against ITS OWN params after the
// constants are raised, and only a re-hash on next login adopts the new cost.
const (
	argonMemory  = 19456 // KiB (19 MiB)
	argonTime    = 2     // passes
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ErrMalformedHash is returned when a stored password hash is not a PHC argon2id
// string this package can parse.
var ErrMalformedHash = errors.New("userstore: malformed password hash")

// HashPassword hashes a plaintext password with argon2id and returns a PHC string
// of the form $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash> (standard, parseable,
// self-describing).
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("userstore: generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether password matches the stored PHC hash. The
// comparison is constant-time. A malformed hash returns ErrMalformedHash (never a
// silent false-as-if-wrong-password) so a corrupted record is distinguishable from
// a bad password.
func VerifyPassword(password, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrMalformedHash
	}
	var m uint32
	var t, p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
