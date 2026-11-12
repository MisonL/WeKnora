package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stubWSProviderRepo struct {
	interfaces.WebSearchProviderRepository
	provider *types.WebSearchProviderEntity
	updates  []*types.WebSearchProviderEntity
}

func (r *stubWSProviderRepo) GetByID(_ context.Context, tenantID uint64, id string) (*types.WebSearchProviderEntity, error) {
	if r.provider == nil || r.provider.TenantID != tenantID || r.provider.ID != id {
		return nil, nil
	}
	cp := *r.provider
	return &cp, nil
}

func (r *stubWSProviderRepo) Update(_ context.Context, p *types.WebSearchProviderEntity) error {
	cp := *p
	r.updates = append(r.updates, &cp)
	return nil
}

type stubWSProviderService struct {
	interfaces.WebSearchProviderService
	repo *stubWSProviderRepo
}

func (s *stubWSProviderService) UpdateProvider(_ context.Context, p *types.WebSearchProviderEntity) error {
	cp := *p
	s.repo.updates = append(s.repo.updates, &cp)
	return nil
}

func (s *stubWSProviderService) CreateProvider(_ context.Context, _ *types.WebSearchProviderEntity) error {
	return nil
}

func existingWSProvider() *types.WebSearchProviderEntity {
	return &types.WebSearchProviderEntity{
		ID:          "p-1",
		TenantID:    1,
		Name:        "Prod Bing",
		Provider:    types.WebSearchProviderTypeBing,
		Description: "primary",
		Parameters: types.WebSearchProviderParameters{
			APIKey:      "stored-key",
			EngineID:    "engine-1",
			BaseURL:     "https://base",
			ProxyURL:    "http://proxy:8080",
			ExtraConfig: map[string]string{"k": "v"},
		},
		IsDefault: false,
	}
}

func wsProviderRequest(t *testing.T, h *WebSearchProviderHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(1))
		c.Next()
	})
	r.PUT("/web-search-providers/:id", h.UpdateProvider)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/web-search-providers/p-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// A partial PUT must keep stored parameters it does not carry and must not
// send an unqualified is_default.
func TestUpdateProviderPreservesOmittedFields(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantCode      int
		wantProxyURL  string
		wantBaseURL   string
		wantEngineID  string
		wantExtraKey  bool
		wantIsDefault bool
	}{
		{
			name:          "only proxy_url",
			body:          `{"parameters":{"proxy_url":"http://new:1"}}`,
			wantCode:      http.StatusOK,
			wantProxyURL:  "http://new:1",
			wantBaseURL:   "https://base",
			wantEngineID:  "engine-1",
			wantExtraKey:  true,
			wantIsDefault: false,
		},
		{
			name:          "only is_default true",
			body:          `{"is_default":true}`,
			wantCode:      http.StatusOK,
			wantProxyURL:  "http://proxy:8080",
			wantBaseURL:   "https://base",
			wantEngineID:  "engine-1",
			wantExtraKey:  true,
			wantIsDefault: true,
		},
		{
			name:          "empty body preserves everything",
			body:          `{}`,
			wantCode:      http.StatusOK,
			wantProxyURL:  "http://proxy:8080",
			wantBaseURL:   "https://base",
			wantEngineID:  "engine-1",
			wantExtraKey:  true,
			wantIsDefault: false,
		},
		{
			name:          "explicit empty proxy_url clears it",
			body:          `{"parameters":{"proxy_url":""}}`,
			wantCode:      http.StatusOK,
			wantProxyURL:  "",
			wantBaseURL:   "https://base",
			wantEngineID:  "engine-1",
			wantExtraKey:  true,
			wantIsDefault: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := existingWSProvider()
			repo := &stubWSProviderRepo{provider: existing}
			h := &WebSearchProviderHandler{repo: repo, service: &stubWSProviderService{repo: repo}}
			w := wsProviderRequest(t, h, tc.body)
			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			require.Len(t, repo.updates, 1)
			got := repo.updates[0]
			require.Equal(t, tc.wantProxyURL, got.Parameters.ProxyURL)
			require.Equal(t, tc.wantBaseURL, got.Parameters.BaseURL)
			require.Equal(t, tc.wantEngineID, got.Parameters.EngineID)
			if tc.wantExtraKey {
				require.Equal(t, map[string]string{"k": "v"}, got.Parameters.ExtraConfig)
			}
			require.Equal(t, "stored-key", got.Parameters.APIKey, "api_key must never flow through PUT")
			require.Equal(t, tc.wantIsDefault, got.IsDefault)
			require.Equal(t, "Prod Bing", got.Name)
			require.Equal(t, "primary", got.Description)
		})
	}
}
