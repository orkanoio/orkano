package auth

import (
	"strings"
	"testing"
)

func TestGenerateRecoveryCodes(t *testing.T) {
	const n = 10
	plain, hashes, err := GenerateRecoveryCodes(n)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(plain) != n {
		t.Fatalf("got %d plaintext codes, want %d", len(plain), n)
	}
	if len(hashes) != n {
		t.Fatalf("got %d hashes, want %d", len(hashes), n)
	}

	seenPlain := map[string]bool{}
	seenHash := map[string]bool{}
	for i, p := range plain {
		if seenPlain[p] {
			t.Fatalf("duplicate plaintext code %q", p)
		}
		seenPlain[p] = true
		if seenHash[hashes[i]] {
			t.Fatalf("duplicate hash for code %q", p)
		}
		seenHash[hashes[i]] = true

		if hashes[i] != HashRecoveryCode(p) {
			t.Fatalf("hashes[%d] does not match HashRecoveryCode(plain[%d])", i, i)
		}
		if strings.Count(p, "-") != recoveryGroups-1 {
			t.Fatalf("code %q has unexpected group separators", p)
		}
	}
}

func TestGenerateRecoveryCodesRejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, _, err := GenerateRecoveryCodes(n); err == nil {
			t.Fatalf("GenerateRecoveryCodes(%d) = nil error, want error", n)
		}
	}
}

func TestHashRecoveryCodeNormalization(t *testing.T) {
	base := HashRecoveryCode("ABCD-EFGH-JKLM-NPQR")

	tests := []struct {
		name string
		code string
		same bool
	}{
		{"identical", "ABCD-EFGH-JKLM-NPQR", true},
		{"lowercase", "abcd-efgh-jklm-npqr", true},
		{"no-separators", "ABCDEFGHJKLMNPQR", true},
		{"spaces", "ABCD EFGH JKLM NPQR", true},
		{"mixed-case-and-spaces", "AbCd efGH jklM NpqR", true},
		{"different-code", "ZZZZ-EFGH-JKLM-NPQR", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HashRecoveryCode(tc.code)
			if tc.same && got != base {
				t.Fatalf("HashRecoveryCode(%q) = %q, want == base %q", tc.code, got, base)
			}
			if !tc.same && got == base {
				t.Fatalf("HashRecoveryCode(%q) unexpectedly equals base", tc.code)
			}
		})
	}
}

func TestHashRecoveryCodeDeterministic(t *testing.T) {
	const code = "WXYZ-1234-5678-9ABC"
	h1 := HashRecoveryCode(code)
	h2 := HashRecoveryCode(code)
	if h1 != h2 {
		t.Fatal("HashRecoveryCode must be deterministic")
	}
}
