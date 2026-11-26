package types

import (
	"log"
	"strings"

	"github.com/Tencent/WeKnora/internal/utils"
)

// secretFieldKeySet is the normalized key set whose values carry credentials
// inside free-form string maps (ModelParameters.ExtraConfig).
//
// Keys are normalized like the access logger does (lowercase, "_"/"-"
// removed), so secret_key / secretKey / secret-key compare equal. Matching is
// exact: max_tokens or tokenizer must not be swept up by a "token" substring.
var secretFieldKeySet = map[string]struct{}{
	"secretkey":   {},
	"apikey":      {},
	"apisecret":   {},
	"token":       {},
	"password":    {},
	"accesstoken": {},
}

// normalizeSecretFieldKey lowercases a key and drops "_"/"-" separators.
func normalizeSecretFieldKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isSensitiveExtraConfigKey reports whether an ExtraConfig key carries a
// credential.
func isSensitiveExtraConfigKey(key string) bool {
	_, sensitive := secretFieldKeySet[normalizeSecretFieldKey(key)]
	return sensitive
}

// isSensitiveMCPHeader reports whether an MCP header carries authentication
// material: authorization (including X-/Proxy- variants), any *api-key, or any
// *token* / *secret* name.
func isSensitiveMCPHeader(name string) bool {
	normalized := normalizeSecretFieldKey(name)
	if strings.Contains(normalized, "authorization") || strings.HasSuffix(normalized, "apikey") {
		return true
	}
	// Cookie is a standard authentication carrier (session=<secret>) and must
	// be encrypted at rest like any other credential header.
	if normalized == "cookie" {
		return true
	}
	return strings.Contains(normalized, "token") || strings.Contains(normalized, "secret")
}

// encryptSecretMapValues returns a copy of in with values of sensitive keys
// encrypted. The input map is never mutated: callers keep the plaintext for
// the live request path. When SYSTEM_AES_KEY is unset the map is returned
// unchanged (desktop/local development keeps the historical plaintext store).
func encryptSecretMapValues(in map[string]string, sensitive func(string) bool) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	key := utils.GetAESKey()
	if key == nil {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == "" || !sensitive(k) {
			out[k] = v
			continue
		}
		encrypted, err := utils.EncryptAESGCM(v, key)
		if err != nil {
			return nil, err
		}
		out[k] = encrypted
	}
	return out, nil
}

// decryptSecretMapValues decrypts sensitive values in place after a row load.
// Legacy plaintext (no enc:v1: prefix) is returned as-is. A value that cannot
// be decrypted is kept as ciphertext: the next Value() writes it back
// unchanged instead of destroying a credential the original key may recover.
func decryptSecretMapValues(in map[string]string, sensitive func(string) bool, label string) {
	for k, v := range in {
		if v == "" || !sensitive(k) {
			continue
		}
		if plain, ok := utils.DecryptStoredSecretLenient(v); ok {
			in[k] = plain
			continue
		}
		log.Printf("[crypto] %s.%s: decrypt failed (SYSTEM_AES_KEY missing/rotated?), keeping ciphertext for write-back",
			label, k)
	}
}
