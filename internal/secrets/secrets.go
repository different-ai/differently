// Package secrets seals provider credentials that operators store in
// Differently, so a Daytona or Freestyle API key can be added through the API
// instead of a Kubernetes Secret.
//
// Values are encrypted with AES-256-GCM under a key-encryption key that never
// enters the database: it comes from an environment variable or a mounted
// file (a Kubernetes Secret, Azure Key Vault through its CSI driver). The
// secret's name is bound in as associated data, so a sealed value cannot be
// moved to another name. A database dump alone reveals no credential.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is the length of the key-encryption key, in bytes.
const KeySize = 32

// ErrNoKey reports that no key-encryption key is configured, so values cannot
// be stored; env: and file: references keep working without one.
var ErrNoKey = errors.New("no secrets key configured: set the secrets key to store provider credentials in Differently")

// Box seals and opens secret values.
type Box struct{ aead cipher.AEAD }

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Load reads a base64 key from the environment variable env or, if set, the
// file path. It returns (nil, nil) when neither holds a key.
func Load(env, file string) (*Box, error) {
	raw := ""
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	case env != "":
		raw = os.Getenv(env)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("secrets key is not base64: %w", err)
	}
	return New(key)
}

// NewKey returns a fresh base64 key for operators to store.
func NewKey() string {
	b := make([]byte, KeySize)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// Seal encrypts value for the secret called name.
func (b *Box) Seal(name, value string) ([]byte, error) {
	if b == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(value), []byte(name)), nil
}

// Open decrypts a value sealed for name.
func (b *Box) Open(name string, sealed []byte) (string, error) {
	if b == nil {
		return "", ErrNoKey
	}
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed secret is too short")
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %s cannot be opened with this key: %w", name, err)
	}
	return string(out), nil
}

// Fingerprint identifies a value without revealing it, so operators can tell
// which version a pool is using.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}
