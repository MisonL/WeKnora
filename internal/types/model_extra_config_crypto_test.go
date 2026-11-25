package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ExtraConfig carries vendor credentials (LKEAP / Volcengine secret_key, and
// other providers' api_key/token/password) in a plain map. Value must encrypt
// the sensitive values and Scan must return them as plaintext to callers.
func TestModelParametersExtraConfigSecretsEncrypted(t *testing.T) {
	withAESKey(t, testAESKey32)
	params := ModelParameters{
		ExtraConfig: map[string]string{
			"secret_key":   "extra-secret-key",
			"api_key":      "extra-api-key",
			"api_secret":   "extra-api-secret",
			"token":        "extra-token",
			"password":     "extra-password",
			"access_token": "extra-access-token",
			"region":       "ap-guangzhou",
			"max_tokens":   "1024",
		},
	}

	raw, err := params.Value()
	require.NoError(t, err)
	encoded := string(raw.([]byte))
	for _, secret := range []string{
		"extra-secret-key", "extra-api-key", "extra-api-secret",
		"extra-token", "extra-password", "extra-access-token",
	} {
		assert.NotContains(t, encoded, secret)
	}
	assert.Contains(t, encoded, "enc:v1:")
	assert.Contains(t, encoded, `"region":"ap-guangzhou"`, "non-sensitive values stay readable")
	assert.Contains(t, encoded, `"max_tokens":"1024"`, "non-secret lookalike keys are untouched")

	// The caller's map must not be rewritten with ciphertext.
	assert.Equal(t, "extra-secret-key", params.ExtraConfig["secret_key"])

	var loaded ModelParameters
	require.NoError(t, loaded.Scan(raw.([]byte)))
	assert.Equal(t, "extra-secret-key", loaded.ExtraConfig["secret_key"])
	assert.Equal(t, "extra-api-key", loaded.ExtraConfig["api_key"])
	assert.Equal(t, "extra-api-secret", loaded.ExtraConfig["api_secret"])
	assert.Equal(t, "extra-token", loaded.ExtraConfig["token"])
	assert.Equal(t, "extra-password", loaded.ExtraConfig["password"])
	assert.Equal(t, "extra-access-token", loaded.ExtraConfig["access_token"])
	assert.Equal(t, "ap-guangzhou", loaded.ExtraConfig["region"])
	assert.Equal(t, "1024", loaded.ExtraConfig["max_tokens"])
}

func TestModelParametersExtraConfigAcceptsLegacyPlaintext(t *testing.T) {
	withAESKey(t, testAESKey32)
	legacy := []byte(`{"extra_config":{"secret_key":"legacy-plain","region":"ap-guangzhou"}}`)

	var loaded ModelParameters
	require.NoError(t, loaded.Scan(legacy))
	assert.Equal(t, "legacy-plain", loaded.ExtraConfig["secret_key"])
	assert.Equal(t, "ap-guangzhou", loaded.ExtraConfig["region"])
}

func TestModelParametersExtraConfigValueWithoutKeyStaysPlaintext(t *testing.T) {
	withAESKey(t, "")
	params := ModelParameters{ExtraConfig: map[string]string{"secret_key": "plain"}}

	raw, err := params.Value()
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(raw.([]byte), &stored))
	assert.Equal(t, "plain", stored["extra_config"].(map[string]any)["secret_key"])
}
