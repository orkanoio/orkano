package auth

import (
	"encoding/base64"
	"testing"
)

func TestNewSessionToken(t *testing.T) {
	raw, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("raw token is not RawURLEncoding base64: %v", err)
	}
	if len(decoded) != sessionTokenBytes {
		t.Fatalf("raw token decodes to %d bytes, want %d", len(decoded), sessionTokenBytes)
	}

	if hash != HashToken(raw) {
		t.Fatal("returned hash must equal HashToken(raw)")
	}
}

func TestNewSessionTokenUnique(t *testing.T) {
	a, _, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	b, _, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if a == b {
		t.Fatal("two session tokens must differ")
	}
}

func TestHashTokenDeterministic(t *testing.T) {
	const raw = "some-opaque-token-value"
	h1 := HashToken(raw)
	h2 := HashToken(raw)
	if h1 != h2 {
		t.Fatal("HashToken must be deterministic")
	}
	if HashToken(raw) == HashToken(raw+"x") {
		t.Fatal("different inputs must hash differently")
	}
	// 32-byte sha256 → 64 hex chars.
	if len(HashToken(raw)) != 64 {
		t.Fatalf("HashToken length = %d, want 64 hex chars", len(HashToken(raw)))
	}
}
