package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	preserveKeyOriginal = "0123456789abcdef0123456789abcdef"
	preserveKeyRotated  = "fedcba9876543210fedcba9876543210"
)

// jsonField extracts one nested string field for assertions.
func jsonField(t *testing.T, raw []byte, path ...string) string {
	t.Helper()
	var current any
	require.NoError(t, json.Unmarshal(raw, &current))
	for _, key := range path {
		object, ok := current.(map[string]any)
		require.Truef(t, ok, "path %v is not an object at %q", path, key)
		current = object[key]
	}
	value, ok := current.(string)
	require.Truef(t, ok, "path %v is not a string", path)
	return value
}

// A rotated SYSTEM_AES_KEY makes a secret unreadable, but a later full-row
// save must write the original ciphertext back rather than blank the column.
func TestModelParametersPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := ModelParameters{APIKey: "sk-live", AppSecret: "app-secret-live"}
	stored, err := original.Value()
	require.NoError(t, err)
	storedBytes := stored.([]byte)
	apiKeyCipher := jsonField(t, storedBytes, "api_key")
	appSecretCipher := jsonField(t, storedBytes, "app_secret")
	require.NotContains(t, string(storedBytes), "sk-live")

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded ModelParameters
	require.NoError(t, loaded.Scan(storedBytes))
	require.Empty(t, loaded.APIKey)
	require.Empty(t, loaded.AppSecret)

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	rewrittenBytes := rewritten.([]byte)
	require.Equal(t, apiKeyCipher, jsonField(t, rewrittenBytes, "api_key"),
		"an unrelated save must restore the stored ciphertext")
	require.Equal(t, appSecretCipher, jsonField(t, rewrittenBytes, "app_secret"))

	// A value assembled for an update (fresh request parameters) gets the
	// markers carried over explicitly.
	var assembled ModelParameters
	assembled.BaseURL = "https://new.example"
	assembled.PreserveUndecryptableSecretsFrom(loaded)
	rebuilt, err := assembled.Value()
	require.NoError(t, err)
	require.Equal(t, apiKeyCipher, jsonField(t, rebuilt.([]byte), "api_key"))

	// A re-entered credential wins over the preserved ciphertext. It is
	// encrypted with whatever key is configured at write time.
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	loaded.APIKey = "sk-new"
	updated, err := loaded.Value()
	require.NoError(t, err)
	newKey := jsonField(t, updated.([]byte), "api_key")
	require.NotEqual(t, apiKeyCipher, newKey)
	var decrypted ModelParameters
	require.NoError(t, decrypted.Scan(updated.([]byte)))
	require.Equal(t, "sk-new", decrypted.APIKey)
}

func TestParserEngineConfigPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := &ParserEngineConfig{
		MinerUAPIKey:          "mineru-key",
		MinerUServerAPIKey:    "mineru-server-key",
		PaddleOCRVLCloudToken: "paddle-token",
	}
	stored, err := original.Value()
	require.NoError(t, err)
	storedBytes := stored.([]byte)
	keys := map[string]string{
		"mineru_api_key":           jsonField(t, storedBytes, "mineru_api_key"),
		"mineru_server_api_key":    jsonField(t, storedBytes, "mineru_server_api_key"),
		"paddleocr_vl_cloud_token": jsonField(t, storedBytes, "paddleocr_vl_cloud_token"),
	}

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded ParserEngineConfig
	require.NoError(t, loaded.Scan(storedBytes))
	require.Empty(t, loaded.MinerUAPIKey)

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	for field, ciphertext := range keys {
		require.Equalf(t, ciphertext, jsonField(t, rewritten.([]byte), field),
			"%s must be restored instead of blanked", field)
	}
}

func TestStorageEngineConfigPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := &StorageEngineConfig{OSS: &OSSEngineConfig{AccessKey: "oss-ak", SecretKey: "oss-sk"}}
	stored, err := original.Value()
	require.NoError(t, err)
	storedBytes := stored.([]byte)
	akCipher := jsonField(t, storedBytes, "oss", "access_key")
	skCipher := jsonField(t, storedBytes, "oss", "secret_key")

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded StorageEngineConfig
	require.NoError(t, loaded.Scan(storedBytes))
	require.Empty(t, loaded.OSS.AccessKey)

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	require.Empty(t, loaded.OSS.AccessKey, "Value must not write ciphertext into the live config")
	require.Equal(t, akCipher, jsonField(t, rewritten.([]byte), "oss", "access_key"))
	require.Equal(t, skCipher, jsonField(t, rewritten.([]byte), "oss", "secret_key"))

	// The settings merge path carries the markers from the stored row.
	merged := MergeStorageEngineConfigForUpdate(&StorageEngineConfig{DefaultProvider: "oss", OSS: &OSSEngineConfig{}}, &loaded)
	mergedOut, err := merged.Value()
	require.NoError(t, err)
	require.Equal(t, akCipher, jsonField(t, mergedOut.([]byte), "oss", "access_key"))
}

func TestMCPAuthConfigPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := &MCPAuthConfig{APIKey: "mcp-key", Token: "mcp-token", ClientSecret: "mcp-secret"}
	stored, err := original.Value()
	require.NoError(t, err)
	storedBytes := stored.([]byte)
	apiKeyCipher := jsonField(t, storedBytes, "api_key")
	tokenCipher := jsonField(t, storedBytes, "token")
	secretCipher := jsonField(t, storedBytes, "client_secret")

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded MCPAuthConfig
	require.NoError(t, loaded.Scan(storedBytes))
	require.Empty(t, loaded.APIKey)

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	require.Equal(t, apiKeyCipher, jsonField(t, rewritten.([]byte), "api_key"))
	require.Equal(t, tokenCipher, jsonField(t, rewritten.([]byte), "token"))
	require.Equal(t, secretCipher, jsonField(t, rewritten.([]byte), "client_secret"))

	// An explicit "drop secrets" must actually clear the column.
	loaded.ClearPreservedSecrets()
	cleared, err := loaded.Value()
	require.NoError(t, err)
	var clearedMap map[string]any
	require.NoError(t, json.Unmarshal(cleared.([]byte), &clearedMap))
	require.NotContains(t, clearedMap, "api_key", "cleared secrets must not be restored")
}

func TestWebSearchProviderParametersPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := WebSearchProviderParameters{APIKey: "search-key", EngineID: "e1"}
	stored, err := original.Value()
	require.NoError(t, err)
	apiKeyCipher := jsonField(t, stored.([]byte), "api_key")

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded WebSearchProviderParameters
	require.NoError(t, loaded.Scan(stored.([]byte)))
	require.Empty(t, loaded.APIKey)

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	require.Equal(t, apiKeyCipher, jsonField(t, rewritten.([]byte), "api_key"))

	var assembled WebSearchProviderParameters
	assembled.PreserveUndecryptableSecretsFrom(loaded)
	require.Equal(t, apiKeyCipher, jsonField(t, mustValue(t, assembled), "api_key"))
}

func TestTenantCredentialsAndAPIPrincipalPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	credentials := &CredentialsConfig{WeKnoraCloud: &WeKnoraCloudCredentials{AppID: "app", AppSecret: "cloud-secret"}}
	storedCredentials, err := credentials.Value()
	require.NoError(t, err)
	cloudSecretCipher := jsonField(t, storedCredentials.([]byte), "weknoracloud", "app_secret")

	principal := &APIPrincipalConfig{Mode: APIPrincipalModeSignedToken, HMACSecret: "hmac-secret"}
	storedPrincipal, err := principal.Value()
	require.NoError(t, err)
	hmacCipher := jsonField(t, storedPrincipal.([]byte), "hmac_secret")

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loadedCredentials CredentialsConfig
	require.NoError(t, loadedCredentials.Scan(storedCredentials.([]byte)))
	require.Empty(t, loadedCredentials.WeKnoraCloud.AppSecret)
	rewrittenCredentials, err := loadedCredentials.Value()
	require.NoError(t, err)
	require.Equal(t, cloudSecretCipher, jsonField(t, rewrittenCredentials.([]byte), "weknoracloud", "app_secret"))

	var loadedPrincipal APIPrincipalConfig
	require.NoError(t, loadedPrincipal.Scan(storedPrincipal.([]byte)))
	require.Empty(t, loadedPrincipal.HMACSecret)
	rewrittenPrincipal, err := loadedPrincipal.Value()
	require.NoError(t, err)
	require.Equal(t, hmacCipher, jsonField(t, rewrittenPrincipal.([]byte), "hmac_secret"))
}

func TestTenantSandboxConfigPreserveUndecryptableSecrets(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := &TenantSandboxConfig{
		SandboxType: "cube",
		Cube:        &CubeSandboxConfig{APIURL: "http://cube:5000", APIKey: "cube-key"},
		EnvVars:     map[string]string{"API_TOKEN": "env-secret"},
		Network: &SandboxNetworkPolicy{
			CubeRules: []CubeEgressRule{{
				Name:   "api",
				Host:   "api.example.com",
				Inject: []CubeHeaderInject{{Header: "Authorization", Secret: "header-secret"}},
			}},
		},
	}
	stored, err := original.Value()
	require.NoError(t, err)
	storedBytes := stored.([]byte)
	cubeCipher := jsonField(t, storedBytes, "cube", "api_key")
	envCipher := jsonField(t, storedBytes, "env_vars", "API_TOKEN")
	var storedDoc map[string]any
	require.NoError(t, json.Unmarshal(storedBytes, &storedDoc))
	network := storedDoc["network"].(map[string]any)
	rules := network["cube_rules"].([]any)
	inject := rules[0].(map[string]any)["inject"].([]any)
	headerCipher := inject[0].(map[string]any)["secret"].(string)
	require.NotEmpty(t, headerCipher)

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var loaded TenantSandboxConfig
	require.NoError(t, loaded.Scan(storedBytes))
	require.Empty(t, loaded.Cube.APIKey)
	require.Empty(t, loaded.EnvVars["API_TOKEN"])

	rewritten, err := loaded.Value()
	require.NoError(t, err)
	rewrittenBytes := rewritten.([]byte)
	require.Equal(t, cubeCipher, jsonField(t, rewrittenBytes, "cube", "api_key"))
	require.Equal(t, envCipher, jsonField(t, rewrittenBytes, "env_vars", "API_TOKEN"))
	require.Contains(t, string(rewrittenBytes), headerCipher,
		"injected header ciphertext must be restored")
}

func TestDataSourceConfigPreserveUndecryptableCredentials(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", preserveKeyOriginal)
	original := &DataSourceConfig{
		Type:        "feishu",
		Credentials: map[string]any{"app_secret": "ds-secret", "app_id": "cli"},
	}
	storedJSON, err := original.ToJSON()
	require.NoError(t, err)
	var storedMap map[string]any
	require.NoError(t, json.Unmarshal(storedJSON, &storedMap))
	credentials := storedMap["credentials"].(map[string]any)
	cipher := credentials["app_secret"].(string)
	require.NotEmpty(t, cipher)

	t.Setenv("SYSTEM_AES_KEY", preserveKeyRotated)
	var source DataSource
	source.Config = storedJSON
	loaded, err := source.ParseConfig()
	require.NoError(t, err)
	require.Empty(t, loaded.Credentials["app_secret"])

	rewritten, err := loaded.ToJSON()
	require.NoError(t, err)
	var rewrittenMap map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &rewrittenMap))
	require.Equal(t, cipher, rewrittenMap["credentials"].(map[string]any)["app_secret"])

	// The update merge path carries the markers onto the new config.
	merged := &DataSourceConfig{Type: "feishu", Credentials: loaded.Credentials}
	merged.PreserveUndecryptableCredentialsFrom(loaded)
	mergedJSON, err := merged.ToJSON()
	require.NoError(t, err)
	var mergedMap map[string]any
	require.NoError(t, json.Unmarshal(mergedJSON, &mergedMap))
	require.Equal(t, cipher, mergedMap["credentials"].(map[string]any)["app_secret"])
}

func mustValue(t *testing.T, params WebSearchProviderParameters) []byte {
	t.Helper()
	raw, err := params.Value()
	require.NoError(t, err)
	return raw.([]byte)
}
