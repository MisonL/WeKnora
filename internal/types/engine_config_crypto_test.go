package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEngineAESKey = "0123456789abcdef0123456789abcdef" // exactly 32 bytes

func storageConfigWithSecrets() *StorageEngineConfig {
	return &StorageEngineConfig{
		DefaultProvider: "oss",
		MinIO:           &MinIOEngineConfig{Endpoint: "minio:9000", AccessKeyID: "minio-ak", SecretAccessKey: "minio-sk", BucketName: "b"},
		COS:             &COSEngineConfig{SecretID: "cos-id", SecretKey: "cos-key", Region: "r", BucketName: "b"},
		TOS:             &TOSEngineConfig{AccessKey: "tos-ak", SecretKey: "tos-sk"},
		S3:              &S3EngineConfig{AccessKey: "s3-ak", SecretKey: "s3-sk"},
		OSS:             &OSSEngineConfig{AccessKey: "oss-ak", SecretKey: "oss-sk", Endpoint: "oss-cn-hangzhou.aliyuncs.com", Region: "cn-hangzhou", BucketName: "b"},
		KS3:             &KS3EngineConfig{AccessKey: "ks3-ak", SecretKey: "ks3-sk"},
		OBS:             &OBSEngineConfig{AccessKey: "obs-ak", SecretKey: "obs-sk"},
	}
}

func TestStorageEngineConfigValueEncryptsCredentials(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", testEngineAESKey)
	cfg := storageConfigWithSecrets()

	raw, err := cfg.Value()
	require.NoError(t, err)

	encoded := string(raw.([]byte))
	require.True(t, strings.Contains(encoded, "enc:v1:"), "stored JSON must contain ciphertext")

	var stored map[string]any
	require.NoError(t, json.Unmarshal(raw.([]byte), &stored))
	ossCfg := stored["oss"].(map[string]any)
	assert.True(t, strings.HasPrefix(ossCfg["access_key"].(string), "enc:v1:"))
	assert.True(t, strings.HasPrefix(ossCfg["secret_key"].(string), "enc:v1:"))

	// Value must not mutate the in-memory config used by the application.
	assert.Equal(t, "oss-ak", cfg.OSS.AccessKey, "Value must not encrypt the live object")
}

func TestStorageEngineConfigScanRoundTrips(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", testEngineAESKey)
	cfg := storageConfigWithSecrets()

	raw, err := cfg.Value()
	require.NoError(t, err)

	var loaded StorageEngineConfig
	require.NoError(t, loaded.Scan(raw.([]byte)))

	assert.Equal(t, "minio-ak", loaded.MinIO.AccessKeyID)
	assert.Equal(t, "minio-sk", loaded.MinIO.SecretAccessKey)
	assert.Equal(t, "cos-id", loaded.COS.SecretID)
	assert.Equal(t, "cos-key", loaded.COS.SecretKey)
	assert.Equal(t, "tos-ak", loaded.TOS.AccessKey)
	assert.Equal(t, "s3-sk", loaded.S3.SecretKey)
	assert.Equal(t, "oss-ak", loaded.OSS.AccessKey)
	assert.Equal(t, "oss-sk", loaded.OSS.SecretKey)
	assert.Equal(t, "ks3-ak", loaded.KS3.AccessKey)
	assert.Equal(t, "obs-sk", loaded.OBS.SecretKey)
}

func TestStorageEngineConfigScanAcceptsLegacyPlaintext(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", testEngineAESKey)
	legacy := []byte(`{"default_provider":"oss","oss":{"endpoint":"e","region":"r","access_key":"plain-ak","secret_key":"plain-sk","bucket_name":"b"}}`)

	var loaded StorageEngineConfig
	require.NoError(t, loaded.Scan(legacy))
	assert.Equal(t, "plain-ak", loaded.OSS.AccessKey, "rows written before encryption must still load")
	assert.Equal(t, "plain-sk", loaded.OSS.SecretKey)
}

func TestStorageEngineConfigScanClearsUndecryptableValues(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", testEngineAESKey)
	// A value with the encryption prefix but produced by a different key.
	broken := []byte(`{"oss":{"access_key":"enc:v1:AAAA","secret_key":"enc:v1:BBBB"}}`)

	var loaded StorageEngineConfig
	require.NoError(t, loaded.Scan(broken))
	assert.Empty(t, loaded.OSS.AccessKey)
	assert.Empty(t, loaded.OSS.SecretKey)
}

func TestParserEngineConfigValueEncryptsSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", testEngineAESKey)
	cfg := &ParserEngineConfig{
		MinerUAPIKey:          "mineru-key",
		MinerUServerAPIKey:    "mineru-server-key",
		PaddleOCRVLCloudToken: "paddle-token",
	}

	raw, err := cfg.Value()
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw.([]byte)), "enc:v1:"))
	assert.Equal(t, "mineru-key", cfg.MinerUAPIKey, "Value must not mutate the live object")

	var loaded ParserEngineConfig
	require.NoError(t, loaded.Scan(raw.([]byte)))
	assert.Equal(t, "mineru-key", loaded.MinerUAPIKey)
	assert.Equal(t, "mineru-server-key", loaded.MinerUServerAPIKey)
	assert.Equal(t, "paddle-token", loaded.PaddleOCRVLCloudToken)
}

func TestEngineConfigValueWithoutKeyStaysPlaintext(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", "")
	cfg := &StorageEngineConfig{OSS: &OSSEngineConfig{AccessKey: "ak", SecretKey: "sk"}}

	raw, err := cfg.Value()
	require.NoError(t, err)
	assert.NotContains(t, string(raw.([]byte)), "enc:v1:", "no key configured preserves the historical plaintext behavior")
}
