package auth

import (
	"encoding/base64"
	"testing"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

// TestHashTokenMatchesPlatformSecrets pins the two independent definitions of
// the bootstrap token's stored form together. `orkano bootstrap-token` writes
// platformsecrets.HashToken's output; the redeem path compares this one. A
// silent divergence would mint tokens that always 401 and read as user error.
func TestHashTokenMatchesPlatformSecrets(t *testing.T) {
	for _, in := range []string{"", "orkano", "a-token-with-symbols_-", "0123456789"} {
		if got, want := HashToken(in), platformsecrets.HashToken(in); got != want {
			t.Errorf("HashToken(%q) = %q, platformsecrets.HashToken = %q", in, got, want)
		}
	}
}

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
