package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// recoveryAlphabet is a Crockford-base32 set with the ambiguous I, L, O, U
// removed, so codes are easy to read aloud and transcribe.
const recoveryAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ0123456789"

// recoveryGroups and recoveryGroupLen shape each code as GROUPS of GROUPLEN
// characters joined by "-": 4×4 = 16 characters from the 32-symbol alphabet =
// 16*log2(32) = 80 bits of entropy, the floor the spec requires.
const (
	recoveryGroups   = 4
	recoveryGroupLen = 4
)

// GenerateRecoveryCodes returns n single-use recovery codes plus their hashes.
// Only the hashes are stored (HashRecoveryCode); the plaintext is shown to the
// admin once and never again — a key-plus-DB-dump still cannot recover them,
// which is strictly safer than encrypting them. Each code has ~80 bits of
// entropy (16 chars over a 32-symbol alphabet).
func GenerateRecoveryCodes(n int) (plaintext []string, hashes []string, err error) {
	if n <= 0 {
		return nil, nil, fmt.Errorf("recovery code count must be positive, got %d", n)
	}
	plaintext = make([]string, 0, n)
	hashes = make([]string, 0, n)
	for i := 0; i < n; i++ {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		plaintext = append(plaintext, code)
		hashes = append(hashes, HashRecoveryCode(code))
	}
	return plaintext, hashes, nil
}

// newRecoveryCode draws recoveryGroups*recoveryGroupLen symbols uniformly from
// recoveryAlphabet and groups them with "-" separators.
func newRecoveryCode() (string, error) {
	total := recoveryGroups * recoveryGroupLen
	buf := make([]byte, total)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating recovery code: %w", err)
	}
	chars := make([]byte, total)
	for i, b := range buf {
		chars[i] = recoveryAlphabet[int(b)%len(recoveryAlphabet)]
	}
	groups := make([]string, recoveryGroups)
	for g := 0; g < recoveryGroups; g++ {
		groups[g] = string(chars[g*recoveryGroupLen : (g+1)*recoveryGroupLen])
	}
	return strings.Join(groups, "-"), nil
}

// HashRecoveryCode normalizes a code (strip separators/spaces, uppercase) then
// returns hex(sha256). Normalization makes the hash insensitive to how the user
// types the code, so the same code always hashes identically at verify time.
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		switch r {
		case '-', ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.ToUpper(b.String())
}
