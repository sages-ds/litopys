package executors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The provider keys a person enters on the settings page never go to the journal (a secret in an append-only log
// cannot be erased): they are sealed with the app key LITOPYS_SECRET_KEY (32 bytes, base64, made at
// install time and kept with the app's secrets) and stored outside the projections, like token hashes.

// ErrNoAppKey: the app has no LITOPYS_SECRET_KEY, so keys cannot be stored here (use secret_ref instead).
var ErrNoAppKey = errors.New("LITOPYS_SECRET_KEY is not set: provider keys cannot be stored by the app")

// Sealer seals and opens provider keys with the app key.
type Sealer struct{ aead cipher.AEAD }

// NewSealer reads the app key: base64 of 32 bytes, or any secret of 32+ characters (an installer's generated
// password) whose SHA-256 is the key; empty gives ErrNoAppKey.
func NewSealer(appKey string) (*Sealer, error) {
	appKey = strings.TrimSpace(appKey)
	if appKey == "" {
		return nil, ErrNoAppKey
	}
	k, err := base64.StdEncoding.DecodeString(appKey)
	if err != nil || len(k) != 32 {
		if len(appKey) < 32 {
			return nil, fmt.Errorf("LITOPYS_SECRET_KEY: base64 of 32 bytes or a secret of 32+ characters expected")
		}
		h := sha256.Sum256([]byte(appKey))
		k = h[:]
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts a key; aad binds it to its executor, so a sealed key moved to another row does not open.
func (s *Sealer) Seal(key string, aad []byte) (nonce, sealed []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, s.aead.Seal(nil, nonce, []byte(key), aad), nil
}

// Open decrypts a sealed key.
func (s *Sealer) Open(nonce, sealed, aad []byte) (string, error) {
	b, err := s.aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return "", errors.New("the sealed key does not open with this app key")
	}
	return string(b), nil
}

// Fingerprint names a key without revealing it: the first 8 hex digits of its SHA-256 — what the journal and the
// page show ("which key is set"), never the key.
func Fingerprint(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:4])
}
