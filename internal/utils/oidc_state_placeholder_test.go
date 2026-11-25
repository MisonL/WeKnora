package utils

import (
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetOIDCStateSecretForTest(t *testing.T, jwtSecret string) {
	t.Helper()
	t.Setenv("JWT_SECRET", jwtSecret)
	oidcStateSecretOnce = sync.Once{}
	oidcStateSecret = ""
	oidcStateSecretErr = nil
}

func TestSignOIDCStateRejectsPlaceholderJWTSecret(t *testing.T) {
	// The value shipped in example configs is public, so signing state
	// with it is equivalent to having no signature at all. Refuse rather
	// than silently falling back to a random per-process key (which
	// would break state verification across instances).
	for _, placeholder := range []string{"weknora-jwt-secret", "CHANGE-ME-jwt-secret", "change-me-jwt-secret"} {
		t.Run(placeholder, func(t *testing.T) {
			resetOIDCStateSecretForTest(t, placeholder)

			_, err := SignOIDCState(&OIDCStatePayload{
				Nonce:       "nonce",
				RedirectURI: "https://app.example/callback",
			})
			if err == nil || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("SignOIDCState(placeholder %q) err = %v, want placeholder rejection", placeholder, err)
			}

			// Verification must refuse too, otherwise a state minted
			// elsewhere with the placeholder would still be accepted.
			payload := `{"nonce":"n","redirect_uri":"https://app.example","iat":` +
				strconv.FormatInt(time.Now().Unix(), 10) + `}`
			raw := base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".AAAA"
			if _, err := VerifyOIDCState(raw); err == nil ||
				!strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("VerifyOIDCState(placeholder %q) err = %v, want placeholder rejection", placeholder, err)
			}
		})
	}
}

func TestSignOIDCStateAcceptsRealSecret(t *testing.T) {
	resetOIDCStateSecretForTest(t, "a-real-oidc-state-secret")

	state, err := SignOIDCState(&OIDCStatePayload{
		Nonce:       "nonce",
		RedirectURI: "https://app.example/callback",
	})
	if err != nil {
		t.Fatalf("SignOIDCState: %v", err)
	}
	if _, err := VerifyOIDCState(state); err != nil {
		t.Fatalf("VerifyOIDCState: %v", err)
	}
}
