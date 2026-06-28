package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const plain = "correct-horse-battery"

	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == plain {
		t.Fatal("hash must not equal the plaintext")
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash %q does not look like a bcrypt hash", hash)
	}

	if err := VerifyPassword(hash, plain); err != nil {
		t.Fatalf("VerifyPassword should match: %v", err)
	}
	if err := VerifyPassword(hash, "wrong-password-here"); err == nil {
		t.Fatal("VerifyPassword should reject a wrong password")
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	const plain = "correct-horse-battery"
	a, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password should differ (per-hash salt)")
	}
}

func TestValidatePasswordPolicy(t *testing.T) {
	tests := []struct {
		name    string
		plain   string
		wantErr bool
	}{
		{"strong", "a-very-strong-passphrase", false},
		{"exactly-min", strings.Repeat("a", MinPasswordLength), false},
		{"one-under-min", strings.Repeat("a", MinPasswordLength-1), true},
		{"empty", "", true},
		{"too-long", strings.Repeat("a", 73), true},
		{"max-72", strings.Repeat("a", 72), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePasswordPolicy(tc.plain)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidatePasswordPolicy(%q) = nil, want error", tc.plain)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidatePasswordPolicy(%q) = %v, want nil", tc.plain, err)
			}
		})
	}
}
