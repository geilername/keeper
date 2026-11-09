package lrlink

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Sealer encrypts tokens at rest with AES-256-GCM. The key is
// KEEPER_TOKEN_KEY: 32 random bytes, base64 (`openssl rand -base64 32`).
// A sealed value is nonce || ciphertext+tag; the additional data binds it to
// where it is stored, so a value copied from Postgres into the KV bucket (or
// the other way round) does not open.
type Sealer struct{ aead cipher.AEAD }

// ErrNoKey is returned when KEEPER_TOKEN_KEY is not set.
var ErrNoKey = errors.New("KEEPER_TOKEN_KEY is not set")

// NewSealer parses a base64 (standard or URL, padded or not) 32-byte key.
func NewSealer(b64 string) (*Sealer, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, ErrNoKey
	}
	var key []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err = enc.DecodeString(b64); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("KEEPER_TOKEN_KEY: not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("KEEPER_TOKEN_KEY: %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plain, bound to aad.
func (s *Sealer) Seal(plain, aad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return s.aead.Seal(nonce, nonce, plain, aad)
}

// ErrOpen is returned for a value that does not decrypt: wrong key,
// tampered, or sealed for another place.
var ErrOpen = errors.New("sealed token does not open (wrong KEEPER_TOKEN_KEY?)")

// Open decrypts a value from Seal with the same aad.
func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, ErrOpen
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
