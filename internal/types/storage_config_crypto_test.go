package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The legacy cos_config column is not covered by SYSTEM_AES_KEY encryption at
// the column level, so StorageConfig itself must encrypt the COS credentials
// before json.Marshal and decrypt them on the way back.
func TestStorageConfigValueEncryptsCOSCredentials(t *testing.T) {
	withAESKey(t, testAESKey32)
	cfg := StorageConfig{
		Provider:   "cos",
		SecretID:   "cos-secret-id",
		SecretKey:  "cos-secret-key",
		Region:     "ap-guangzhou",
		BucketName: "bucket-1",
		AppID:      "app-1",
		PathPrefix: "exports",
	}

	raw, err := cfg.Value()
	require.NoError(t, err)
	encoded := string(raw.([]byte))

	assert.NotContains(t, encoded, "cos-secret-id")
	assert.NotContains(t, encoded, "cos-secret-key")
	assert.Contains(t, encoded, "enc:v1:")

	var stored StorageConfig
	require.NoError(t, json.Unmarshal(raw.([]byte), &stored))
	assert.True(t, strings.HasPrefix(stored.SecretID, "enc:v1:"))
	assert.True(t, strings.HasPrefix(stored.SecretKey, "enc:v1:"))

	// The in-memory value used by the application must stay plaintext.
	assert.Equal(t, "cos-secret-id", cfg.SecretID)
	assert.Equal(t, "cos-secret-key", cfg.SecretKey)
}

func TestStorageConfigScanRoundTrips(t *testing.T) {
	withAESKey(t, testAESKey32)
	cfg := StorageConfig{SecretID: "roundtrip-id", SecretKey: "roundtrip-key", BucketName: "b"}
	raw, err := cfg.Value()
	require.NoError(t, err)

	var loaded StorageConfig
	require.NoError(t, loaded.Scan(raw.([]byte)))
	assert.Equal(t, "roundtrip-id", loaded.SecretID)
	assert.Equal(t, "roundtrip-key", loaded.SecretKey)
	assert.Equal(t, "b", loaded.BucketName)
}

func TestStorageConfigScanAcceptsLegacyPlaintext(t *testing.T) {
	withAESKey(t, testAESKey32)
	legacy := []byte(`{"provider":"cos","secret_id":"plain-id","secret_key":"plain-key","bucket_name":"b"}`)

	var loaded StorageConfig
	require.NoError(t, loaded.Scan(legacy))
	assert.Equal(t, "plain-id", loaded.SecretID, "rows written before encryption must still load")
	assert.Equal(t, "plain-key", loaded.SecretKey)
}

func TestStorageConfigValueWithoutKeyStaysPlaintext(t *testing.T) {
	withAESKey(t, "")
	cfg := StorageConfig{SecretID: "ak", SecretKey: "sk"}

	raw, err := cfg.Value()
	require.NoError(t, err)
	assert.NotContains(t, string(raw.([]byte)), "enc:v1:", "no key configured keeps the historical plaintext behavior")
}
