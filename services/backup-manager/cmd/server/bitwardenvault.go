package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// A self-hosted Bitwarden-compatible vault (Vaultwarden) has no Secrets
// Manager, so bws cannot read from it. The drill reads the backup key from a
// vault item instead, the way a Bitwarden client does: it signs in with the
// account's personal API key, derives the master key from the master
// password with the account's KDF, unwraps the account key, and decrypts the
// one item. Nothing is written back to the vault.
//
//	SCOS_BW_SERVER_URL           the vault, e.g. https://vault.example
//	SCOS_BW_CLIENT_ID            the account's API key client_id (user.…)
//	SCOS_BW_CLIENT_SECRET_FILE   file holding the API key client_secret
//	SCOS_BW_PASSWORD_FILE        file holding the account's master password
//	SCOS_BACKUP_KEY_SECRET_ID    the item's id; the key is its password,
//	                             or its notes when it has no password

// vaultConfig is a configured vault, or nil when SCOS_BW_SERVER_URL is unset.
type vaultConfig struct {
	serverURL    string
	clientID     string
	clientSecret string
	password     string
}

func vaultDrillConfig() (*vaultConfig, error) {
	server := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_BW_SERVER_URL")), "/")
	if server == "" {
		return nil, nil
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("SCOS_BW_SERVER_URL %q is not an https URL", server)
	}
	c := &vaultConfig{serverURL: server, clientID: strings.TrimSpace(os.Getenv("SCOS_BW_CLIENT_ID"))}
	if c.clientID == "" {
		return nil, errors.New("SCOS_BW_CLIENT_ID names no vault API key")
	}
	for _, f := range []struct {
		env string
		dst *string
	}{{"SCOS_BW_CLIENT_SECRET_FILE", &c.clientSecret}, {"SCOS_BW_PASSWORD_FILE", &c.password}} {
		path := strings.TrimSpace(os.Getenv(f.env))
		if path == "" {
			return nil, fmt.Errorf("%s is not set", f.env)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.env, err)
		}
		// A master password may begin or end with spaces; only the line
		// ending a file editor adds is dropped.
		*f.dst = strings.TrimRight(string(b), "\r\n")
		if *f.dst == "" {
			return nil, fmt.Errorf("%s is empty", f.env)
		}
	}
	return c, nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// vaultKey is a symmetric key: 32 bytes of AES-256 and 32 of HMAC-SHA256.
type vaultKey struct{ enc, mac []byte }

func vaultKeyFrom(b []byte) (vaultKey, error) {
	if len(b) != 64 {
		return vaultKey{}, fmt.Errorf("a vault key is 64 bytes, not %d", len(b))
	}
	return vaultKey{enc: b[:32], mac: b[32:]}, nil
}

// kdfParams are the account's master-key derivation settings.
type kdfParams struct {
	Kdf            int `json:"kdf"`
	KdfIterations  int `json:"kdfIterations"`
	KdfMemory      int `json:"kdfMemory"`
	KdfParallelism int `json:"kdfParallelism"`
}

// masterKey derives the account's master key from the password, salted
// with the trimmed, lower-cased email, under the account's KDF. The
// parameter bounds are the ones Bitwarden clients enforce.
func masterKey(password, email string, p kdfParams) ([]byte, error) {
	salt := strings.ToLower(strings.TrimSpace(email))
	switch p.Kdf {
	case 0:
		if p.KdfIterations < 5000 {
			return nil, fmt.Errorf("PBKDF2 with %d iterations is below the 5000 Bitwarden accepts", p.KdfIterations)
		}
		return pbkdf2.Key(sha256.New, password, []byte(salt), p.KdfIterations, 32)
	case 1:
		if p.KdfIterations < 2 || p.KdfMemory < 16 || p.KdfMemory > 1024 || p.KdfParallelism < 1 || p.KdfParallelism > 16 {
			return nil, fmt.Errorf("Argon2id parameters t=%d m=%dMiB p=%d are outside what Bitwarden accepts", p.KdfIterations, p.KdfMemory, p.KdfParallelism)
		}
		s := sha256.Sum256([]byte(salt))
		return argon2.IDKey([]byte(password), s[:], uint32(p.KdfIterations), uint32(p.KdfMemory)*1024, uint8(p.KdfParallelism), 32), nil
	}
	return nil, fmt.Errorf("unknown KDF type %d", p.Kdf)
}

// stretchedMasterKey is the key that wraps the account key: the master key
// expanded with HKDF-SHA256 into an encryption and a MAC key.
func stretchedMasterKey(password, email string, p kdfParams) (vaultKey, error) {
	master, err := masterKey(password, email, p)
	if err != nil {
		return vaultKey{}, err
	}
	enc, err := hkdf.Expand(sha256.New, master, "enc", 32)
	if err != nil {
		return vaultKey{}, err
	}
	mac, err := hkdf.Expand(sha256.New, master, "mac", 32)
	if err != nil {
		return vaultKey{}, err
	}
	return vaultKey{enc: enc, mac: mac}, nil
}

// decryptEncString opens a type 2 EncString, "2.iv|data|mac": AES-256-CBC
// whose HMAC-SHA256 over iv||data is checked before anything is decrypted.
// Types without a MAC are refused.
func decryptEncString(s string, k vaultKey) ([]byte, error) {
	kind, body, ok := strings.Cut(s, ".")
	if !ok || kind != "2" {
		return nil, fmt.Errorf("encrypted value of type %q is not AES-256-CBC with HMAC-SHA256", kind)
	}
	parts := strings.Split(body, "|")
	if len(parts) != 3 {
		return nil, errors.New("encrypted value is not iv|data|mac")
	}
	var raw [3][]byte
	for i, p := range parts {
		b, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("encrypted value is not base64: %w", err)
		}
		raw[i] = b
	}
	iv, data, tag := raw[0], raw[1], raw[2]
	m := hmac.New(sha256.New, k.mac)
	m.Write(iv)
	m.Write(data)
	if !hmac.Equal(m.Sum(nil), tag) {
		return nil, errors.New("encrypted value fails its MAC: wrong key or tampered data")
	}
	if len(iv) != aes.BlockSize || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted value has a malformed iv or length")
	}
	block, err := aes.NewCipher(k.enc)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("encrypted value has bad padding")
	}
	return out[:len(out)-pad], nil
}

// decryptRSAEncString opens an organisation key wrapped to the account's
// RSA key: type 3 (OAEP SHA-256) or type 4 (OAEP SHA-1).
func decryptRSAEncString(s string, priv *rsa.PrivateKey) ([]byte, error) {
	kind, body, ok := strings.Cut(s, ".")
	if !ok {
		return nil, errors.New("wrapped key has no type")
	}
	data, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("wrapped key is not base64: %w", err)
	}
	switch kind {
	case "3":
		return rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, data, nil)
	case "4":
		return rsa.DecryptOAEP(sha1.New(), rand.Reader, priv, data, nil)
	}
	return nil, fmt.Errorf("wrapped key of type %q is not RSA-OAEP", kind)
}

type vaultToken struct {
	AccessToken string `json:"access_token"`
	kdfParams
}

type vaultCipher struct {
	ID             string  `json:"id"`
	OrganizationID *string `json:"organizationId"`
	Type           int     `json:"type"`
	Key            *string `json:"key"`
	Name           string  `json:"name"`
	Notes          *string `json:"notes"`
	Login          *struct {
		Password *string `json:"password"`
	} `json:"login"`
	RevisionDate string  `json:"revisionDate"`
	DeletedDate  *string `json:"deletedDate"`
}

type vaultSync struct {
	Profile struct {
		Email         string `json:"email"`
		Key           string `json:"key"`
		PrivateKey    string `json:"privateKey"`
		Organizations []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"organizations"`
	} `json:"profile"`
	Ciphers []vaultCipher `json:"ciphers"`
}

// vaultDeviceID is this deployment's device identifier, kept beside the
// backups so every drill signs in as the same device rather than
// registering a new one each time.
func (s *backupServer) vaultDeviceID() (string, error) {
	path := filepath.Join(s.backupDir, "secret-drill-device-id")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) == 36 {
		return strings.TrimSpace(string(b)), nil
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	if err := os.MkdirAll(s.backupDir, 0o750); err != nil {
		return "", err
	}
	return id, writeAtomic(path, []byte(id+"\n"), 0o600)
}

var vaultHTTPClient = &http.Client{Timeout: time.Minute}

// vaultClientVersion is the Bitwarden client release whose API the drill
// speaks.
const vaultClientVersion = "2025.10.0"

func vaultRequest(ctx context.Context, req *http.Request, into any) error {
	req = req.WithContext(ctx)
	req.Header.Set("Accept", "application/json")
	// Vaultwarden refuses some requests from clients that do not name
	// themselves the way Bitwarden's own clients do.
	req.Header.Set("Bitwarden-Client-Name", "cli")
	req.Header.Set("Bitwarden-Client-Version", vaultClientVersion)
	resp, err := vaultHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Message          string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		msg := strings.TrimSpace(strings.Join([]string{e.Error, e.ErrorDescription, e.Message}, " "))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, msg)
	}
	return json.Unmarshal(body, into)
}

// fetchVaultSecret signs in to the vault with the API key and returns the
// decrypted item.
func (s *backupServer) fetchVaultSecret(ctx context.Context, c *vaultConfig, itemID string) (keySecret, error) {
	var sec keySecret
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	device, err := s.vaultDeviceID()
	if err != nil {
		return sec, fmt.Errorf("device identifier: %w", err)
	}
	form := url.Values{
		"grant_type":       {"client_credentials"},
		"scope":            {"api"},
		"client_id":        {c.clientID},
		"client_secret":    {c.clientSecret},
		"deviceType":       {"25"}, // Linux CLI
		"deviceIdentifier": {device},
		"deviceName":       {"scos-backup-manager"},
	}
	req, err := http.NewRequest(http.MethodPost, c.serverURL+"/identity/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return sec, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok vaultToken
	if err := vaultRequest(ctx, req, &tok); err != nil {
		return sec, fmt.Errorf("vault sign-in: %w", err)
	}
	if tok.AccessToken == "" {
		return sec, errors.New("vault sign-in returned no access token")
	}

	req, err = http.NewRequest(http.MethodGet, c.serverURL+"/api/sync?excludeDomains=true", nil)
	if err != nil {
		return sec, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	var sync vaultSync
	if err := vaultRequest(ctx, req, &sync); err != nil {
		return sec, fmt.Errorf("vault sync: %w", err)
	}

	kdf := tok.kdfParams
	if kdf.KdfIterations == 0 {
		body, _ := json.Marshal(map[string]string{"email": sync.Profile.Email})
		req, err = http.NewRequest(http.MethodPost, c.serverURL+"/identity/accounts/prelogin", bytes.NewReader(body))
		if err != nil {
			return sec, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := vaultRequest(ctx, req, &kdf); err != nil {
			return sec, fmt.Errorf("vault KDF settings: %w", err)
		}
	}
	stretched, err := stretchedMasterKey(c.password, sync.Profile.Email, kdf)
	if err != nil {
		return sec, err
	}
	raw, err := decryptEncString(sync.Profile.Key, stretched)
	if err != nil {
		return sec, fmt.Errorf("the master password does not unlock the vault account: %w", err)
	}
	userKey, err := vaultKeyFrom(raw)
	if err != nil {
		return sec, fmt.Errorf("account key: %w", err)
	}

	var item *vaultCipher
	for i := range sync.Ciphers {
		if sync.Ciphers[i].ID == itemID {
			item = &sync.Ciphers[i]
			break
		}
	}
	switch {
	case item == nil:
		return sec, fmt.Errorf("the vault has no item %s visible to this account", itemID)
	case item.DeletedDate != nil:
		return sec, fmt.Errorf("vault item %s is in the trash", itemID)
	}

	key := userKey
	if item.OrganizationID != nil && *item.OrganizationID != "" {
		if key, err = organisationKey(sync, *item.OrganizationID, userKey); err != nil {
			return sec, err
		}
	}
	if item.Key != nil && *item.Key != "" {
		raw, err := decryptEncString(*item.Key, key)
		if err != nil {
			return sec, fmt.Errorf("item key: %w", err)
		}
		if key, err = vaultKeyFrom(raw); err != nil {
			return sec, fmt.Errorf("item key: %w", err)
		}
	}
	open := func(field, v string) (string, error) {
		b, err := decryptEncString(v, key)
		if err != nil {
			return "", fmt.Errorf("item %s: %w", field, err)
		}
		return string(b), nil
	}
	if sec.Key, err = open("name", item.Name); err != nil {
		return sec, err
	}
	switch {
	case item.Login != nil && item.Login.Password != nil && *item.Login.Password != "":
		sec.Value, err = open("password", *item.Login.Password)
	case item.Notes != nil && *item.Notes != "":
		sec.Value, err = open("notes", *item.Notes)
	default:
		err = fmt.Errorf("vault item %s (%s) has neither a password nor notes", itemID, sec.Key)
	}
	if err != nil {
		return sec, err
	}
	sec.ID, sec.RevisionDate = item.ID, item.RevisionDate
	return sec, nil
}

// organisationKey unwraps an organisation's key with the account's RSA key.
func organisationKey(sync vaultSync, orgID string, userKey vaultKey) (vaultKey, error) {
	var wrapped string
	for _, o := range sync.Profile.Organizations {
		if o.ID == orgID {
			wrapped = o.Key
		}
	}
	if wrapped == "" {
		return vaultKey{}, fmt.Errorf("the account is not a member of organisation %s", orgID)
	}
	der, err := decryptEncString(sync.Profile.PrivateKey, userKey)
	if err != nil {
		return vaultKey{}, fmt.Errorf("account private key: %w", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return vaultKey{}, fmt.Errorf("account private key: %w", err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return vaultKey{}, errors.New("account private key is not RSA")
	}
	raw, err := decryptRSAEncString(wrapped, priv)
	if err != nil {
		return vaultKey{}, fmt.Errorf("organisation %s key: %w", orgID, err)
	}
	return vaultKeyFrom(raw)
}
