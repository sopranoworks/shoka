package userstore

import (
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpPeriod is the standard RFC 6238 step (30s). totpSkew=1 accepts the adjacent
// step on each side (±1 step) to tolerate clock drift, as the directive specifies.
const (
	totpPeriod = 30
	totpSkew   = 1
)

// VerifyTOTP reports whether code is a valid current TOTP for the user's enrolled
// secret, evaluated at now with ±1-step skew. Returns (false, nil) for a user who
// has not enrolled TOTP. The secret is decrypted from the record using the store's
// key (the plaintext never leaves this call).
func (s *Store) VerifyTOTP(rec *UserRecord, code string, now time.Time) (bool, error) {
	secret, err := s.openTOTP(rec.TOTPSecretEnc)
	if err != nil {
		return false, err
	}
	if secret == "" {
		return false, nil
	}
	return totp.ValidateCustom(code, secret, now, totp.ValidateOpts{
		Period:    totpPeriod,
		Skew:      totpSkew,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
}

// GenerateTOTP creates a fresh TOTP secret for email under issuer (the RP display
// name). It returns the otp.Key whose Secret() is the base32 secret to seal and
// whose String() is the otpauth:// provisioning URI for the authenticator QR.
func GenerateTOTP(issuer, email string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: email,
		Period:      totpPeriod,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
}
