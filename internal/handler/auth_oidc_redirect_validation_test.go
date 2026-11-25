package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
)

// newOIDCURLTestRouter wires /auth/oidc/url with a service stub that
// records the redirect_uri it was handed.
func newOIDCURLTestRouter(t *testing.T, frontendBaseURL string) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	captured := ""
	us := &stubOIDCStartUserService{
		getOIDCAuthorizationURL: func(_ context.Context, redirectURI string) (*types.OIDCAuthURLResponse, error) {
			captured = redirectURI
			return &types.OIDCAuthURLResponse{
				Success:          true,
				AuthorizationURL: "https://idp.example/authorize",
				Nonce:            "n",
			}, nil
		},
	}
	h := NewAuthHandler(&config.Config{FrontendBaseURL: frontendBaseURL}, us, nil, nil, nil)
	r := gin.New()
	r.Use(errorCapture())
	r.GET("/auth/oidc/url", h.GetOIDCAuthorizationURL)
	return r, &captured
}

func callOIDCURL(t *testing.T, r *gin.Engine, redirectURI string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/url?redirect_uri="+url.QueryEscape(redirectURI), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestOIDCURLAllowsSameOriginRedirect(t *testing.T) {
	r, captured := newOIDCURLTestRouter(t, "https://app.example")

	w := callOIDCURL(t, r, "https://app.example/callback")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if *captured != "https://app.example/callback" {
		t.Fatalf("redirect_uri = %q, want %q", *captured, "https://app.example/callback")
	}
}

func TestOIDCURLAllowsRelativeRedirect(t *testing.T) {
	r, captured := newOIDCURLTestRouter(t, "https://app.example")

	w := callOIDCURL(t, r, "/login/callback")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if *captured != "/login/callback" {
		t.Fatalf("redirect_uri = %q, want /login/callback", *captured)
	}
}

func TestOIDCURLRejectsForeignRedirect(t *testing.T) {
	cases := []struct {
		name        string
		baseURL     string
		redirectURI string
	}{
		{"different host", "https://app.example", "https://evil.example/callback"},
		{"different scheme", "https://app.example", "http://app.example/callback"},
		{"different port", "https://app.example", "https://app.example:8443/callback"},
		{"protocol relative", "https://app.example", "//evil.example/callback"},
		{"subdomain", "https://app.example", "https://evil.app.example/callback"},
		{"userinfo trick", "https://app.example", "https://app.example@evil.example/callback"},
		{"absolute with no configured base", "", "https://app.example/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, captured := newOIDCURLTestRouter(t, tc.baseURL)
			w := callOIDCURL(t, r, tc.redirectURI)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if *captured != "" {
				t.Fatalf("service must not be called for a rejected redirect_uri, got %q", *captured)
			}
		})
	}
}

func TestOIDCURLAllowsExplicitDefaultPort(t *testing.T) {
	r, _ := newOIDCURLTestRouter(t, "https://app.example")
	if w := callOIDCURL(t, r, "https://app.example:443/callback"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for explicit default port (body=%s)", w.Code, w.Body.String())
	}
}
