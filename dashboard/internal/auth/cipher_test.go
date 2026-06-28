package auth

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newTestKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestNewCipherKeyValidation(t *testing.T) {
	tests := []struct {
		name    string
		keyB64  string
		wantErr bool
	}{
		{"valid-32-bytes", newTestKey(t), false},
		{"too-short-16-bytes", base64.StdEncoding.EncodeToString(make([]byte, 16)), true},
		{"too-long-64-bytes", base64.StdEncoding.EncodeToString(make([]byte, 64)), true},
		{"empty", "", true},
		{"not-base64", "@@@not-base64@@@", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCipher(tc.keyB64)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewCipher(%q) = nil error, want error", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCipher: %v", err)
			}
			if c == nil {
				t.Fatal("NewCipher returned nil cipher with nil error")
			}
		})
	}
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	const seed = "JBSWY3DPEHPK3PXP"
	sealed, err := c.Seal(seed)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1:") {
		t.Fatalf("sealed value %q missing v1: prefix", sealed)
	}
	if strings.Contains(sealed, seed) {
		t.Fatal("sealed value must not contain the plaintext")
	}

	opened, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != seed {
		t.Fatalf("round-trip mismatch: got %q, want %q", opened, seed)
	}
}

func TestCipherSealUsesFreshNonce(t *testing.T) {
	c, err := NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	a, err := c.Seal("same-plaintext")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := c.Seal("same-plaintext")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if a == b {
		t.Fatal("two Seals of the same plaintext must differ (random nonce)")
	}
}

func TestCipherOpenRejectsBadInput(t *testing.T) {
	c, err := NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	sealed, err := c.Seal("the-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Tampered ciphertext: decode the payload, flip a bit in the GCM tag (the
	// last byte), and re-encode. Flipping a base64 *character* is unreliable —
	// the final RawStdEncoding char carries don't-care trailing bits, so a
	// flipped char can decode to identical bytes and not actually tamper.
	rawPayload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	if err != nil {
		t.Fatalf("decoding sealed payload: %v", err)
	}
	rawPayload[len(rawPayload)-1] ^= 0x01
	tampered := "v1:" + base64.RawStdEncoding.EncodeToString(rawPayload)

	// Wrong key: a second, independent cipher cannot open the first's output.
	other, err := NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher (other): %v", err)
	}

	tests := []struct {
		name   string
		sealed string
		c      *Cipher
	}{
		{"tampered-ciphertext", tampered, c},
		{"wrong-key", sealed, other},
		{"unknown-version", "v2:" + strings.TrimPrefix(sealed, "v1:"), c},
		{"missing-version-prefix", strings.TrimPrefix(sealed, "v1:"), c},
		{"not-base64-payload", "v1:@@@", c},
		{"too-short-for-nonce", "v1:" + base64.RawStdEncoding.EncodeToString([]byte("x")), c},
		{"empty", "", c},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.Open(tc.sealed)
			if err == nil {
				t.Fatalf("Open(%q) = %q, want error", tc.name, got)
			}
			if got != "" {
				t.Fatalf("Open returned %q on error, want empty string", got)
			}
		})
	}
}
