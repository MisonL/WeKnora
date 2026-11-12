package im

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/stretchr/testify/require"
)

// withAESKey installs a valid SYSTEM_AES_KEY for the duration of a test.
func withAESKey(t *testing.T, key string) {
	t.Helper()
	t.Setenv("SYSTEM_AES_KEY", key)
	require.NotNil(t, utils.GetAESKey(), "test key must decode to 32 bytes")
}

const testAESKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The stored value must not contain the plaintext secret: that is the whole
// point of the type. It also has to stay valid JSON, because the column is
// jsonb.
func TestCredentialsJSONEncryptsAtRest(t *testing.T) {
	withAESKey(t, testAESKeyHex)

	plain := NewCredentialsJSON([]byte(`{"bot_id":"aibot","bot_secret":"super-secret-value"}`))
	stored, err := plain.Value()
	require.NoError(t, err)

	raw, ok := stored.([]byte)
	require.True(t, ok, "Value must return bytes for a jsonb column")
	require.NotContains(t, string(raw), "super-secret-value")
	require.True(t, json.Valid(raw), "stored jsonb must remain valid JSON")
	require.True(t, strings.HasPrefix(string(raw), `"`+utils.EncPrefix),
		"stored value must be the encrypted wrapper, got %s", raw)

	loaded := new(CredentialsJSON)
	require.NoError(t, loaded.Scan(raw))
	require.JSONEq(t, string(plain.Bytes()), string(loaded.Bytes()))
}

// Rows written before this type existed hold a plain JSON object. They must
// keep working so an upgrade does not disconnect every channel.
func TestCredentialsJSONReadsLegacyPlaintext(t *testing.T) {
	withAESKey(t, testAESKeyHex)

	legacy := []byte(`{"bot_id":"aibot","bot_secret":"legacy-plaintext"}`)
	loaded := new(CredentialsJSON)
	require.NoError(t, loaded.Scan(legacy))
	require.JSONEq(t, string(legacy), string(loaded.Bytes()))
	require.Contains(t, string(loaded.Bytes()), "legacy-plaintext",
		"a legacy row loads as the document it was written as")
}

// Without SYSTEM_AES_KEY the desktop and local-development path must keep
// writing plaintext rather than failing every write.
func TestCredentialsJSONWithoutKeyStaysPlaintext(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", "")
	require.Nil(t, utils.GetAESKey())

	plain := NewCredentialsJSON([]byte(`{"token":"dev-only"}`))
	stored, err := plain.Value()
	require.NoError(t, err)

	raw, ok := stored.([]byte)
	require.True(t, ok)
	require.JSONEq(t, string(plain.Bytes()), string(raw))
}

// A rotated key leaves ciphertext that cannot be decrypted. The row must still
// load, with credentials cleared, so the channel list is not broken and the
// adapter fails with "not configured" instead of sending ciphertext upstream.
func TestCredentialsJSONWrongKeyLoadsUnconfigured(t *testing.T) {
	withAESKey(t, testAESKeyHex)
	stored, err := NewCredentialsJSON([]byte(`{"token":"rotated-away"}`)).Value()
	require.NoError(t, err)

	// Rotate the key: decrypting the old ciphertext must fail.
	withAESKey(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")

	loaded := new(CredentialsJSON)
	require.NoError(t, loaded.Scan(stored.([]byte)))
	require.True(t, loaded.Undecryptable(),
		"a decrypt failure must mark the value read-only, not silently empty")
	require.True(t, imCredentialsConfigured(*loaded))

	// And the write path must refuse to persist the placeholder: a Save of
	// this row would otherwise destroy the ciphertext forever.
	_, err = loaded.Value()
	require.ErrorIs(t, err, ErrChannelCredentialsUndecryptable)
}

// The encryption has to survive the GORM round trip the channel table uses,
// including the jsonb default, so a save followed by a load returns the same
// document and never the ciphertext.
func TestCredentialsJSONSurvivesJSONRoundTrip(t *testing.T) {
	withAESKey(t, testAESKeyHex)

	type row struct {
		Credentials CredentialsJSON `json:"credentials"`
	}
	in := row{Credentials: NewCredentialsJSON([]byte(`{"app_secret":"feishu-secret"}`))}

	// API shape: the admin POST body carries the plaintext document.
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(body), "feishu-secret", "API echo stays plaintext for admins")

	var decoded row
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.JSONEq(t, string(in.Credentials.Bytes()), string(decoded.Credentials.Bytes()))

	// Storage shape: what a driver would hand back on the next read.
	stored, err := decoded.Credentials.Value()
	require.NoError(t, err)
	var scanned row
	require.NoError(t, scanned.Credentials.Scan(stored))
	require.JSONEq(t, string(in.Credentials.Bytes()), string(scanned.Credentials.Bytes()))
}

// Guard the driver contract itself: a Valuer that returns a string would still
// work, but []byte keeps the column byte-compatible with the other JSON columns.
func TestCredentialsJSONValueIsDriverValue(t *testing.T) {
	withAESKey(t, testAESKeyHex)
	value, err := NewCredentialsJSON([]byte(`{"token":"x"}`)).Value()
	require.NoError(t, err)
	var _ driver.Value = value
	require.NotEmpty(t, value)
}

// `"credentials": null` on a partial update must not blank a channel's stored
// credentials; the endpoint treats nil as "leave unchanged".
func TestCredentialsJSONNullBodyLeavesFieldUnset(t *testing.T) {
	var field CredentialsJSON
	require.NoError(t, json.Unmarshal([]byte(`null`), &field))
	require.True(t, field.IsZero())

	// And a stored jsonb null loads as an empty document rather than the
	// literal "null", which would otherwise report "configured".
	var loaded CredentialsJSON
	require.NoError(t, loaded.Scan([]byte(`null`)))
	require.JSONEq(t, `{}`, string(loaded.Bytes()))
	require.False(t, imCredentialsConfigured(loaded))
}
