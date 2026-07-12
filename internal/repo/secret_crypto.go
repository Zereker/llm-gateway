package repo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
)

// secret_crypto.go provides static package-level AES-256-GCM encryption/decryption:
//
// The only public API:
//
//	SetDataKey(hex)  -- called once at startup (cmd/* loads it from cfg.DataKey)
//
// The internals are used by AuthConfig.Scanner/Valuer — the other typed JSON
// columns (Routing/Quota/Capabilities) aren't encrypted (not sensitive). Any
// future column that needs encryption should also go through this
// encryptBlob/decryptBlob pair.
//
// **Format**: blobPrefix + base64( nonce || ciphertext || tag )
// blobPrefix is "v1:"; a future KEK rotation could add "v2:".
//
// **KEK not set**: encrypt/decrypt return an error (never panic); the error
// propagates up the call stack until startup's CheckSchema fails fast.

const blobPrefix = "v1:"

// aeadAtomic holds a pointer to the cipher.AEAD, published after SetDataKey.
// Uses atomic.Pointer instead of sync.Mutex -- the hot path is read-mostly.
var aeadAtomic atomic.Pointer[cipher.AEAD]

// SetDataKey initializes the AES-256-GCM cipher from a hex-encoded 32-byte key.
//
//	hexKey must be exactly 64 hex characters (= 256 bit); any other length is an error.
//
// Calling it again replaces the previous cipher (doesn't normally happen in
// production; tests may set it repeatedly).
func SetDataKey(hexKey string) error {
	if len(hexKey) != 64 {
		return fmt.Errorf("repo: data_key must be 64 hex chars (got %d)", len(hexKey))
	}

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("repo: data_key hex decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("repo: aes.NewCipher: %w", err)
	}

	a, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("repo: cipher.NewGCM: %w", err)
	}

	aeadAtomic.Store(&a)

	return nil
}

// encryptBlob encrypts plain -> "v1:" + base64(nonce||ct||tag).
//
// A fresh random nonce every time; calling it repeatedly with the same plain
// yields different ciphertext.
func encryptBlob(plain []byte) ([]byte, error) {
	a, err := loadAEAD()
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("repo: rand nonce: %w", err)
	}

	ct := a.Seal(nil, nonce, plain, nil)
	raw := make([]byte, 0, len(nonce)+len(ct))
	raw = append(raw, nonce...)
	raw = append(raw, ct...)
	encoded := base64.StdEncoding.EncodeToString(raw)

	return []byte(blobPrefix + encoded), nil
}

// decryptBlob recovers the plaintext.
func decryptBlob(b []byte) ([]byte, error) {
	a, err := loadAEAD()
	if err != nil {
		return nil, err
	}

	if !bytes.HasPrefix(b, []byte(blobPrefix)) {
		return nil, fmt.Errorf("repo: blob missing %q prefix", blobPrefix)
	}

	encoded := b[len(blobPrefix):]

	raw, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("repo: blob base64: %w", err)
	}

	if len(raw) < a.NonceSize() {
		return nil, errors.New("repo: blob too short for nonce")
	}

	nonce, ct := raw[:a.NonceSize()], raw[a.NonceSize():]

	plain, err := a.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("repo: aead open: %w", err)
	}

	return plain, nil
}

// loadAEAD returns the current cipher.AEAD; errors if nil (KEK not loaded).
func loadAEAD() (cipher.AEAD, error) {
	p := aeadAtomic.Load()
	if p == nil {
		return nil, errors.New("repo: data_key not set; call repo.SetDataKey at startup")
	}

	return *p, nil
}
