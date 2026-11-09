package im

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/Tencent/WeKnora/internal/utils"
)

// CredentialsJSON is the at-rest form of im_channels.credentials. It behaves
// like types.JSON inside the process — callers always see the plaintext
// document — but GORM encrypts the whole document on write and decrypts it on
// read.
//
// The whole document is encrypted instead of a fixed list of key names because
// every IM platform names its secrets differently (bot_secret, app_secret,
// encoding_aes_key, signing_secret, bot_token, outgoing_token, ...). A key
// list would silently store the credential of a platform or field we did not
// anticipate — the exact failure mode that left tenant engine credentials in
// plaintext. A JSON string scalar is a valid jsonb value, and nothing queries
// inside this column (duplicate-bot detection reads the separate bot_identity
// column), so the placeholder shape costs nothing.
//
// Legacy rows written before this type existed are plain JSON objects. Scan
// keeps them usable as-is; they are re-encrypted the next time the channel is
// saved.
type CredentialsJSON json.RawMessage

// Value implements driver.Valuer: it encrypts the document for storage.
//
// An unset SYSTEM_AES_KEY keeps the historical plaintext behaviour used by
// desktop and local development. A configured-but-broken key is a hard error so
// a write never silently stores a plaintext secret.
func (c CredentialsJSON) Value() (driver.Value, error) {
	if len(c) == 0 {
		return nil, nil
	}
	key := utils.GetAESKey()
	if key == nil {
		return []byte(c), nil
	}
	encrypted, err := utils.EncryptAESGCM(string(c), key)
	if err != nil {
		return nil, fmt.Errorf("encrypt im channel credentials: %w", err)
	}
	return json.Marshal(encrypted)
}

// Scan implements sql.Scanner: it decrypts a stored document back to plaintext
// JSON.
func (c *CredentialsJSON) Scan(value any) error {
	if value == nil {
		*c = nil
		return nil
	}

	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("im channel credentials: unsupported scan type %T", value)
	}

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		*c = nil
		return nil
	}

	// A JSON string scalar is the encrypted wrapper. Credential documents are
	// always objects, so anything else means the row predates encryption.
	if raw[0] == '"' {
		var stored string
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("im channel credentials: decode stored value: %w", err)
		}
		plain, ok := utils.DecryptStoredSecretLenient(stored)
		if !ok {
			// Key rotated or removed: load the row as unconfigured rather than
			// failing the whole channel list, matching the other credential
			// loaders. The channel then fails to start with a clear adapter
			// error instead of sending ciphertext upstream.
			log.Printf("[crypto] im channel credentials: decrypt failed (SYSTEM_AES_KEY missing/rotated?), treating as unconfigured")
			*c = CredentialsJSON(`{}`)
			return nil
		}
		if len(plain) == 0 {
			*c = CredentialsJSON(`{}`)
			return nil
		}
		if !json.Valid([]byte(plain)) {
			return errors.New("im channel credentials: decrypted value is not valid JSON")
		}
		*c = CredentialsJSON(plain)
		return nil
	}

	if !json.Valid(raw) {
		return errors.New("im channel credentials: stored value is not valid JSON")
	}
	if string(raw) == "null" {
		*c = CredentialsJSON(`{}`)
		return nil
	}
	copied := make([]byte, len(raw))
	copy(copied, raw)
	*c = CredentialsJSON(copied)
	return nil
}

// MarshalJSON lets the API echo the plaintext document back to an admin.
func (c CredentialsJSON) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(c).MarshalJSON()
}

// UnmarshalJSON accepts the plaintext document from a request body. A JSON
// null leaves the field unset, so `"credentials": null` cannot blank a channel's
// stored credentials through the partial-update endpoint.
func (c *CredentialsJSON) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("CredentialsJSON: UnmarshalJSON on nil pointer")
	}
	if string(bytes.TrimSpace(data)) == "null" {
		*c = nil
		return nil
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	*c = CredentialsJSON(copied)
	return nil
}
