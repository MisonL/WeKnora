package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPHeadersValueEncryptsSensitiveHeaders(t *testing.T) {
	withAESKey(t, testAESKey32)
	headers := MCPHeaders{
		"Authorization":   "Bearer header-secret",
		"X-API-Key":       "header-api-key",
		"X-Request-Token": "header-token",
		"X-Client-Secret": "header-secret-value",
		"Accept":          "application/json",
		"X-Trace-ID":      "trace-1",
	}

	raw, err := headers.Value()
	require.NoError(t, err)

	encoded := string(raw.([]byte))
	for _, secret := range []string{
		"header-secret", "header-api-key", "header-token", "header-secret-value",
	} {
		assert.NotContains(t, encoded, secret)
	}
	assert.Contains(t, encoded, "enc:v1:")
	assert.Contains(t, encoded, `"Accept":"application/json"`)
	assert.Contains(t, encoded, `"X-Trace-ID":"trace-1"`)

	// The caller's map must stay plaintext for the live request path.
	assert.Equal(t, "Bearer header-secret", headers["Authorization"])

	var loaded MCPHeaders
	require.NoError(t, loaded.Scan(raw.([]byte)))
	assert.Equal(t, "Bearer header-secret", loaded["Authorization"])
	assert.Equal(t, "header-api-key", loaded["X-API-Key"])
	assert.Equal(t, "header-token", loaded["X-Request-Token"])
	assert.Equal(t, "header-secret-value", loaded["X-Client-Secret"])
	assert.Equal(t, "application/json", loaded["Accept"])
	assert.Equal(t, "trace-1", loaded["X-Trace-ID"])
}

func TestMCPHeadersScanAcceptsLegacyPlaintext(t *testing.T) {
	withAESKey(t, testAESKey32)
	legacy := MCPHeaders{"Authorization": "Bearer legacy", "Accept": "application/json"}
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)

	var loaded MCPHeaders
	require.NoError(t, loaded.Scan(raw))
	assert.Equal(t, "Bearer legacy", loaded["Authorization"])
	assert.Equal(t, "application/json", loaded["Accept"])
}

func TestMCPAuthConfigCustomHeadersEncrypted(t *testing.T) {
	withAESKey(t, testAESKey32)
	cfg := &MCPAuthConfig{
		APIKey: "auth-api-key",
		CustomHeaders: map[string]string{
			"Authorization":    "Bearer custom-secret",
			"X-API-Key":        "custom-api-key",
			"X-Refresh-Token":  "custom-token",
			"X-Signing-Secret": "custom-signing-secret",
			"X-Tenant-Header":  "tenant-7",
		},
	}

	raw, err := cfg.Value()
	require.NoError(t, err)
	encoded := string(raw.([]byte))
	for _, secret := range []string{
		"custom-secret", "custom-api-key", "custom-token", "custom-signing-secret",
	} {
		assert.NotContains(t, encoded, secret)
	}
	assert.Contains(t, encoded, `"X-Tenant-Header":"tenant-7"`, "non-sensitive custom headers are not encrypted")
	assert.Equal(t, "Bearer custom-secret", cfg.CustomHeaders["Authorization"], "Value must not mutate the live config")

	var loaded MCPAuthConfig
	require.NoError(t, loaded.Scan(raw.([]byte)))
	assert.Equal(t, "auth-api-key", loaded.APIKey)
	assert.Equal(t, "Bearer custom-secret", loaded.CustomHeaders["Authorization"])
	assert.Equal(t, "custom-api-key", loaded.CustomHeaders["X-API-Key"])
	assert.Equal(t, "custom-token", loaded.CustomHeaders["X-Refresh-Token"])
	assert.Equal(t, "custom-signing-secret", loaded.CustomHeaders["X-Signing-Secret"])
	assert.Equal(t, "tenant-7", loaded.CustomHeaders["X-Tenant-Header"])
}

func TestMCPAuthConfigCustomHeadersLegacyPlaintext(t *testing.T) {
	withAESKey(t, testAESKey32)
	legacy := []byte(`{"custom_headers":{"Authorization":"Bearer legacy","X-Trace":"abc"}}`)

	var loaded MCPAuthConfig
	require.NoError(t, loaded.Scan(legacy))
	assert.Equal(t, "Bearer legacy", loaded.CustomHeaders["Authorization"])
	assert.Equal(t, "abc", loaded.CustomHeaders["X-Trace"])
}
