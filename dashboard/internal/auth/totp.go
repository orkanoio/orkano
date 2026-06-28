package auth

import (
	"fmt"

	"github.com/pquerna/otp/totp"
)

// GenerateTOTP creates a new TOTP key for the given issuer (e.g. "Orkano") and
// account (the admin's username). It returns the base32 seed — which the caller
// MUST encrypt (see Cipher) before storing — and the otpauth:// URL the UI
// renders as a QR code for the authenticator app.
func GenerateTOTP(issuer, account string) (secret string, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
	})
	if err != nil {
		return "", "", fmt.Errorf("generating TOTP key: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// ValidateTOTP reports whether code is currently valid for the base32 secret.
// pquerna's default validation allows a one-step skew window, which absorbs
// minor clock drift between the server and the authenticator.
func ValidateTOTP(secret, code string) bool {
	return totp.Validate(code, secret)
}
