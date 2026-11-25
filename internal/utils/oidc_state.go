package utils

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const oidcStateMaxAge = 10 * time.Minute

// OIDCStatePayload is the signed OIDC authorization state carried in the
// redirect URL and validated on callback.
type OIDCStatePayload struct {
	Nonce       string `json:"nonce"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	IssuedAt    int64  `json:"iat"`
	PKCEEnabled bool   `json:"pkce,omitempty"`
}

var (
	oidcStateSecretOnce sync.Once
	oidcStateSecret     string
	oidcStateSecretErr  error
)

// placeholderJWTSecrets are the shipped sample values. Signing state with
// one of them is equivalent to having no secret at all: the value is in
// the repository, so anyone can forge state. service.getJwtSecret ignores
// the same values (falling back to a random key); OIDC state cannot do
// that safely across instances, so it refuses instead.
var placeholderJWTSecrets = map[string]struct{}{
	"weknora-jwt-secret":   {},
	"CHANGE-ME-jwt-secret": {},
	"change-me-jwt-secret": {},
	"your-jwt-secret":      {},
	"your_secret_key_here": {},
}

// oidcStateSigningKey returns the HMAC key for OIDC state. A configured
// JWT_SECRET is used as-is; a placeholder secret is refused rather than
// silently replaced, because state is verified by every instance behind
// the load balancer and a per-process random key would fail validation
// across instances. An unset secret keeps the historical per-process
// random fallback.
func oidcStateSigningKey() (string, error) {
	oidcStateSecretOnce.Do(func() {
		envSecret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
		if _, placeholder := placeholderJWTSecrets[envSecret]; placeholder {
			oidcStateSecretErr = errors.New(
				"JWT_SECRET is set to a published placeholder value; set a real secret before enabling OIDC")
			return
		}
		if envSecret != "" {
			oidcStateSecret = envSecret
			return
		}
		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			oidcStateSecretErr = fmt.Errorf("failed to generate OIDC state signing key: %w", err)
			return
		}
		oidcStateSecret = base64.StdEncoding.EncodeToString(randomBytes)
	})
	return oidcStateSecret, oidcStateSecretErr
}

// SignOIDCState returns a tamper-evident state token: base64url(payload).base64url(hmac).
func SignOIDCState(payload *OIDCStatePayload) (string, error) {
	if payload == nil {
		return "", errors.New("oidc state payload is required")
	}
	if strings.TrimSpace(payload.Nonce) == "" {
		return "", errors.New("oidc state nonce is required")
	}
	if strings.TrimSpace(payload.RedirectURI) == "" {
		return "", errors.New("oidc state redirect_uri is required")
	}
	if payload.IssuedAt == 0 {
		payload.IssuedAt = time.Now().Unix()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal oidc state: %w", err)
	}
	key, err := oidcStateSigningKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(raw)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyOIDCState validates the HMAC and freshness of a state token.
func VerifyOIDCState(raw string) (*OIDCStatePayload, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid oidc state format")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode oidc state payload: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode oidc state signature: %w", err)
	}
	key, err := oidcStateSigningKey()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payloadBytes)
	if !hmac.Equal(mac.Sum(nil), sigBytes) {
		return nil, errors.New("oidc state signature mismatch")
	}
	var payload OIDCStatePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal oidc state: %w", err)
	}
	if strings.TrimSpace(payload.RedirectURI) == "" {
		return nil, errors.New("state.redirect_uri is required")
	}
	if payload.IssuedAt == 0 {
		return nil, errors.New("state.iat is required")
	}
	issuedAt := time.Unix(payload.IssuedAt, 0)
	if time.Since(issuedAt) > oidcStateMaxAge || time.Until(issuedAt) > time.Minute {
		return nil, errors.New("oidc state expired or invalid timestamp")
	}
	return &payload, nil
}
