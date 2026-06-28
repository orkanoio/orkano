package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// cipherVersion tags every sealed blob so the key/algorithm can rotate later: a
// future "v2:" payload can carry a key-id while old "v1:" blobs still decrypt.
const cipherVersion = "v1"

const aes256KeyLen = 32

// Cipher provides AES-256-GCM encryption at rest for the TOTP seed. The seed
// must be DECRYPTABLE to verify codes (unlike a password, which is one-way
// hashed), so it is encrypted under a key held outside the database — a DB dump
// alone cannot recover it.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a base64 (standard encoding) key that decodes
// to EXACTLY 32 bytes (AES-256). A wrong-length key is rejected with a clear
// error rather than silently degrading the cipher strength.
func NewCipher(keyB64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("decoding cipher key: %w", err)
	}
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("cipher key must be %d bytes (AES-256), got %d", aes256KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("building AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("building GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce and returns
// "v1:" + base64.RawStdEncoding(nonce || ciphertext||tag). A unique nonce per
// call is mandatory for GCM, so two Seals of the same plaintext differ.
func (c *Cipher) Seal(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return cipherVersion + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. It rejects an unknown version tag, a truncated payload,
// and — via GCM's authentication tag — any tampering, a wrong key, or a corrupt
// blob; on failure it returns an error and never partial/garbage plaintext.
func (c *Cipher) Open(sealed string) (string, error) {
	version, encoded, ok := strings.Cut(sealed, ":")
	if !ok {
		return "", errors.New("sealed value is missing its version prefix")
	}
	if version != cipherVersion {
		return "", fmt.Errorf("unknown sealed-value version %q", version)
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding sealed value: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("sealed value is too short to contain a nonce")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting sealed value: %w", err)
	}
	return string(plaintext), nil
}
