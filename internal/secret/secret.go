// Package secret provides the keyed primitives the portal stores secrets with:
// AES-256-GCM for reversible ciphertext (vm2api keys, passwords) and
// HMAC-SHA256 for lookup hashes (portal API keys, session tokens).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

type Box struct {
	aead   cipher.AEAD
	macKey []byte
}

func New(masterKey string) (*Box, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("secret key too short")
	}
	// Derive independent subkeys so the same master key never serves two purposes.
	aesKey := derive([]byte(masterKey), "portal/aes-256-gcm")
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Box{aead: aead, macKey: derive([]byte(masterKey), "portal/hmac-sha256")}, nil
}

func derive(key []byte, label string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(label))
	return h.Sum(nil)
}

// Seal encrypts plaintext as base64(nonce||ciphertext).
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(out), nil
}

func (b *Box) Open(sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

// MAC is the stable lookup hash for portal API keys and session tokens.
func (b *Box) MAC(value string) string {
	h := hmac.New(sha256.New, b.macKey)
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

// RandomToken returns n bytes of crypto-random data, base62-ish url-safe text.
func RandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
