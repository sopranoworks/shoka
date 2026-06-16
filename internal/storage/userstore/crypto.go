package userstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrTOTPDecrypt is returned when a stored TOTP secret cannot be decrypted (wrong
// key or a corrupted record).
var ErrTOTPDecrypt = errors.New("userstore: totp secret decrypt failed")

// ResolveTOTPKey derives the 32-byte AES-256 key used to encrypt TOTP secrets at
// rest. Key source, in priority order (the directive §3.6 "pick the simplest
// correct option and STATE it"):
//
//  1. configKey, when non-empty: any operator-supplied string, hashed with SHA-256
//     to a fixed 32-byte key. This lets an operator pin the key (e.g. to share a
//     store across a rebuild) in the config they already edit.
//  2. otherwise: a random 32-byte key generated once and persisted at keyPath
//     (0600). Zero-config — the default deployment needs no key management; the key
//     lives beside the store file and is read on subsequent boots.
//
// This keeps TOTP secrets encrypted at rest without forcing the operator to manage
// a key, while still allowing a pinned key when they want one.
func ResolveTOTPKey(configKey, keyPath string) ([]byte, error) {
	if configKey != "" {
		sum := sha256.Sum256([]byte(configKey))
		return sum[:], nil
	}
	if b, err := os.ReadFile(keyPath); err == nil && len(b) == 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("userstore: generate totp key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return nil, fmt.Errorf("userstore: create key dir: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("userstore: persist totp key: %w", err)
	}
	return key, nil
}

// sealTOTP encrypts a base32 TOTP secret with AES-256-GCM, prefixing the random
// nonce. Returns nil for an empty secret (TOTP not enrolled).
func (s *Store) sealTOTP(secret string) ([]byte, error) {
	if secret == "" {
		return nil, nil
	}
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("userstore: totp nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(secret), nil), nil
}

// openTOTP decrypts a sealed TOTP secret. An empty input yields "" (not enrolled).
func (s *Store) openTOTP(enc []byte) (string, error) {
	if len(enc) == 0 {
		return "", nil
	}
	gcm, err := s.gcm()
	if err != nil {
		return "", err
	}
	if len(enc) < gcm.NonceSize() {
		return "", ErrTOTPDecrypt
	}
	nonce, ct := enc[:gcm.NonceSize()], enc[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrTOTPDecrypt
	}
	return string(pt), nil
}

func (s *Store) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.totpKey[:])
	if err != nil {
		return nil, fmt.Errorf("userstore: aes cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
