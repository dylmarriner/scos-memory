package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
)

// masterKey loads the AES-256 key used to encrypt credential values at
// rest, from SCOS_CREDENTIAL_VAULT_MASTER_KEY (base64-encoded, 32 bytes
// decoded). It is read fresh on every call rather than cached in a package
// variable so a key rotation only requires restarting the process with a
// new environment, not auditing for a stale cached copy. The key itself
// must never be logged or returned in an error.
func masterKey() ([]byte, error) {
	encoded := strings.TrimSpace(os.Getenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY"))
	if encoded == "" {
		return nil, errors.New("SCOS_CREDENTIAL_VAULT_MASTER_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("SCOS_CREDENTIAL_VAULT_MASTER_KEY must be base64-encoded")
	}
	if len(key) != 32 {
		return nil, errors.New("SCOS_CREDENTIAL_VAULT_MASTER_KEY must decode to 32 bytes (AES-256)")
	}
	return key, nil
}

// encryptValue seals plaintext under key with a fresh random nonce. The
// caller stores ciphertext and nonce together; neither is useful without
// the other and neither is useful without key.
func encryptValue(key []byte, plaintext string) (ciphertext, nonce []byte, err error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, []byte(plaintext), nil), nonce, nil
}

// decryptValue is the inverse of encryptValue. A wrong key, a corrupted
// ciphertext, and a tampered ciphertext are all indistinguishable to the
// caller by design -- GCM authentication failure returns one generic error.
func decryptValue(key, ciphertext, nonce []byte) (string, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("credential decryption failed")
	}
	return string(plaintext), nil
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
