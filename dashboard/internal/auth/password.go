// Package auth holds the dashboard's bootstrap-auth primitives (ADR-0003): the
// single local admin's password hashing and policy, AES-256-GCM encryption of
// the TOTP seed at rest, TOTP generation/verification, single-use recovery
// codes, and opaque session tokens. It owns crypto choices only — the session
// lifecycle, lockout, and step-up flows that consume these live in higher M2.3
// layers. Nothing here logs, persists, or holds global mutable state.
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength is the floor enforced by ValidatePasswordPolicy. The single
// local admin sets this once; length is the only knob that meaningfully raises
// cost against offline cracking of a stolen bcrypt hash, so the policy is just a
// minimum length rather than a brittle composition rule.
const MinPasswordLength = 12

// bcryptCost is the work factor for password hashing. 12 is above bcrypt's
// DefaultCost (10) — a deliberate margin for a credential that guards the whole
// control panel.
const bcryptCost = 12

// ErrPasswordTooShort is returned by ValidatePasswordPolicy when the password is
// under MinPasswordLength.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)

// HashPassword returns a bcrypt hash of plain at bcryptCost.
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword reports whether plain matches the bcrypt hash. A nil return
// means a match; a mismatch (or a malformed hash) returns a wrapped error.
func VerifyPassword(hash, plain string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return fmt.Errorf("password verification failed: %w", err)
	}
	return nil
}

// ValidatePasswordPolicy enforces the minimum-length policy. bcrypt silently
// truncates inputs past 72 bytes, so a password that long is rejected too — the
// bytes beyond 72 would be ignored, weakening the credential without the user
// knowing.
func ValidatePasswordPolicy(plain string) error {
	if len(plain) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(plain) > 72 {
		return errors.New("password must be at most 72 bytes (bcrypt ignores any beyond)")
	}
	return nil
}
