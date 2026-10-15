package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Environment variables that configure the vault's keyring. The current key
// seals every new or re-sealed value; previous keys are held only so values
// sealed before a rotation stay readable until POST /v1/admin/keys/rotate has
// re-sealed them under the current version. No key material is ever logged
// or returned in an error -- only version numbers are.
const (
	envMasterKey    = "SCOS_CREDENTIAL_VAULT_MASTER_KEY"
	envKeyVersion   = "SCOS_CREDENTIAL_VAULT_KEY_VERSION"
	envPreviousKeys = "SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS"
)

// defaultKeyVersion matches credentials.key_version's column default
// (database/migrations/000024_credential_vault.sql), so a deployment that
// predates key versioning keeps reading every stored value with no
// configuration change: its single master key is version 1.
const defaultKeyVersion = 1

// keyring is the set of AES-256 keys the vault can open ciphertexts with,
// indexed by the key_version recorded alongside each ciphertext.
type keyring struct {
	current int
	keys    map[int][]byte
}

// unknownKeyVersionError reports a ciphertext sealed under a key version
// this process was not configured with. It names the version (not secret)
// so an operator knows which retired key must be put back into
// SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS.
type unknownKeyVersionError struct {
	version int
}

func (e unknownKeyVersionError) Error() string {
	return fmt.Sprintf("credential sealed with key version %d, which is not configured (add it to %s)", e.version, envPreviousKeys)
}

// loadKeyring reads the keyring from the environment:
//
//	SCOS_CREDENTIAL_VAULT_MASTER_KEY     base64 32-byte current key (required)
//	SCOS_CREDENTIAL_VAULT_KEY_VERSION    positive integer version of that key (default 1)
//	SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS  comma-separated <version>:<base64 key> retired keys
//
// Every version must be unique, and the current version must be the highest
// configured: a "rotation" that seals new values under an older version
// number than values already stored would make version ordering meaningless
// for operators deciding when a retired key can be dropped.
func loadKeyring() (*keyring, error) {
	current, err := decodeKey(envMasterKey, os.Getenv(envMasterKey))
	if err != nil {
		return nil, err
	}
	version := defaultKeyVersion
	if raw := strings.TrimSpace(os.Getenv(envKeyVersion)); raw != "" {
		version, err = parseKeyVersion(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envKeyVersion, err)
		}
	}
	ring := &keyring{current: version, keys: map[int][]byte{version: current}}
	for _, entry := range strings.Split(os.Getenv(envPreviousKeys), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		rawVersion, encoded, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%s entries must be <version>:<base64 key>", envPreviousKeys)
		}
		previousVersion, err := parseKeyVersion(strings.TrimSpace(rawVersion))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envPreviousKeys, err)
		}
		if _, exists := ring.keys[previousVersion]; exists {
			return nil, fmt.Errorf("%s: key version %d is configured more than once", envPreviousKeys, previousVersion)
		}
		if previousVersion > version {
			return nil, fmt.Errorf("%s: previous key version %d is newer than current key version %d", envPreviousKeys, previousVersion, version)
		}
		key, err := decodeKey(fmt.Sprintf("%s version %d", envPreviousKeys, previousVersion), encoded)
		if err != nil {
			return nil, err
		}
		ring.keys[previousVersion] = key
	}
	return ring, nil
}

func parseKeyVersion(raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return 0, errors.New("key version must be a positive integer")
	}
	return v, nil
}

func decodeKey(name, encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64-encoded", name)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes (AES-256)", name)
	}
	return key, nil
}

// currentVersion is the key version every new seal is recorded under.
func (k *keyring) currentVersion() int {
	return k.current
}

// versions lists every configured key version in ascending order.
func (k *keyring) versions() []int {
	out := make([]int, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func (k *keyring) has(version int) bool {
	_, ok := k.keys[version]
	return ok
}

// seal encrypts plaintext under the current key and returns the version the
// caller must store next to the ciphertext.
func (k *keyring) seal(plaintext string) (ciphertext, nonce []byte, version int, err error) {
	ciphertext, nonce, err = encryptValue(k.keys[k.current], plaintext)
	if err != nil {
		return nil, nil, 0, err
	}
	return ciphertext, nonce, k.current, nil
}

// open decrypts a ciphertext with the key matching the version it was
// sealed under.
func (k *keyring) open(version int, ciphertext, nonce []byte) (string, error) {
	key, ok := k.keys[version]
	if !ok {
		return "", unknownKeyVersionError{version: version}
	}
	return decryptValue(key, ciphertext, nonce)
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
