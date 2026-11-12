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

// ErrChannelCredentialsUndecryptable is returned when a write would touch a
// channel whose stored credentials could not be decrypted with the current
// SYSTEM_AES_KEY. The stored ciphertext may still be recoverable with the
// original key, so it must never be overwritten — not even with a fresh
// document — until the operator either restores the key or deletes and
// recreates the channel. The error text must never carry ciphertext or keys.
var ErrChannelCredentialsUndecryptable = errors.New(
	"channel credentials cannot be decrypted with the current SYSTEM_AES_KEY; " +
		"restore the original key, then re-enter the credentials (or recreate the channel)")

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
//
// When decryption fails (SYSTEM_AES_KEY missing or rotated), Scan records the
// fact in the unexported undecryptable flag instead of treating the value as
// "no credentials": the row still loads so listings work, but Value refuses to
// serialize the placeholder, so no save can ever replace unrecoverable
// ciphertext with an empty document.
type CredentialsJSON struct {
	raw           json.RawMessage
	undecryptable bool
}

// NewCredentialsJSON builds a CredentialsJSON from a plaintext document.
func NewCredentialsJSON(raw []byte) CredentialsJSON {
	copied := make([]byte, len(raw))
	copy(copied, raw)
	return CredentialsJSON{raw: copied}
}

// Bytes returns the plaintext document. Nil-safe.
func (c CredentialsJSON) Bytes() []byte {
	return []byte(c.raw)
}

// IsZero reports whether no document has been set (absent request field,
// JSON null, or never scanned).
func (c CredentialsJSON) IsZero() bool {
	return len(c.raw) == 0
}

// Undecryptable reports whether the stored ciphertext failed to decrypt with
// the current SYSTEM_AES_KEY. A value with this flag must never be written
// back: Value returns ErrChannelCredentialsUndecryptable.
func (c CredentialsJSON) Undecryptable() bool {
	return c.undecryptable
}

// Value implements driver.Valuer: it encrypts the document for storage.
//
// An unset SYSTEM_AES_KEY keeps the historical plaintext behaviour used by
// desktop and local development. A configured-but-broken key is a hard error so
// a write never silently stores a plaintext secret. An undecryptable document
// is a hard error as well: writing it back would destroy ciphertext that the
// original key may still recover.
func (c CredentialsJSON) Value() (driver.Value, error) {
	if c.undecryptable {
		return nil, ErrChannelCredentialsUndecryptable
	}
	if len(c.raw) == 0 {
		return nil, nil
	}
	key := utils.GetAESKey()
	if key == nil {
		return []byte(c.raw), nil
	}
	encrypted, err := utils.EncryptAESGCM(string(c.raw), key)
	if err != nil {
		return nil, fmt.Errorf("encrypt im channel credentials: %w", err)
	}
	return json.Marshal(encrypted)
}

// Scan implements sql.Scanner: it decrypts a stored document back to plaintext
// JSON.
func (c *CredentialsJSON) Scan(value any) error {
	if value == nil {
		*c = CredentialsJSON{}
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
		*c = CredentialsJSON{}
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
			// loaders. The undecryptable flag keeps the write path from
			// persisting the placeholder over the still-stored ciphertext.
			log.Printf("[crypto] im channel credentials: decrypt failed (SYSTEM_AES_KEY missing/rotated?), treating as unconfigured and read-only")
			*c = CredentialsJSON{undecryptable: true}
			return nil
		}
		if len(plain) == 0 {
			*c = CredentialsJSON{raw: json.RawMessage(`{}`)}
			return nil
		}
		if !json.Valid([]byte(plain)) {
			return errors.New("im channel credentials: decrypted value is not valid JSON")
		}
		*c = CredentialsJSON{raw: json.RawMessage(plain)}
		return nil
	}

	if !json.Valid(raw) {
		return errors.New("im channel credentials: stored value is not valid JSON")
	}
	if string(raw) == "null" {
		*c = CredentialsJSON{raw: json.RawMessage(`{}`)}
		return nil
	}
	copied := make([]byte, len(raw))
	copy(copied, raw)
	*c = CredentialsJSON{raw: copied}
	return nil
}

// MarshalJSON lets the API echo the plaintext document back to an admin. An
// undecryptable value serializes as null so the sentinel never leaks.
func (c CredentialsJSON) MarshalJSON() ([]byte, error) {
	if len(c.raw) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(c.raw).MarshalJSON()
}

// UnmarshalJSON accepts the plaintext document from a request body. A JSON
// null leaves the field unset, so `"credentials": null` cannot blank a channel's
// stored credentials through the partial-update endpoint.
func (c *CredentialsJSON) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("CredentialsJSON: UnmarshalJSON on nil pointer")
	}
	if string(bytes.TrimSpace(data)) == "null" {
		*c = CredentialsJSON{}
		return nil
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	*c = CredentialsJSON{raw: copied}
	return nil
}
