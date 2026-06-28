package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// sessionTokenBytes is the entropy of an opaque session token: 32 bytes = 256
// bits, well past any feasible guessing budget.
const sessionTokenBytes = 32

// NewSessionToken mints an opaque session token. raw is the value handed to the
// client (the cookie); hash is HashToken(raw), the only form stored server-side
// (ADR-0003) — so a database dump cannot be replayed as a live session.
func NewSessionToken() (raw string, hash string, err error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// HashToken returns hex(sha256(raw)). It is deterministic and general: the
// caller reuses it to compare a presented install/bootstrap token against a
// stored sha256 hash. Constant-time comparison is the caller's responsibility
// (compare the fixed-length hex outputs, not the raw tokens).
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
