package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type chunkingPutKBService struct {
	interfaces.KnowledgeBaseService
	kb        *types.KnowledgeBase
	gotConfig *types.KnowledgeBaseConfig
	gotName   string
}

func (s *chunkingPutKBService) GetKnowledgeBaseByID(context.Context, string) (*types.KnowledgeBase, error) {
	return s.kb, nil
}

func (s *chunkingPutKBService) UpdateKnowledgeBase(
	_ context.Context, _, name, _ string, cfg *types.KnowledgeBaseConfig, _ *types.VLMConfig,
) (*types.KnowledgeBase, error) {
	s.gotConfig = cfg
	s.gotName = name
	return s.kb, nil
}

// A PUT whose config omits chunking_config must hand the service the stored
// chunking settings, not zeros; an explicit chunking_config still replaces.
func TestUpdateKnowledgeBasePUTPreservesChunkingConfig(t *testing.T) {
	existing := types.ChunkingConfig{ChunkSize: 512, ChunkOverlap: 64, Separators: []string{"\n\n"}}
	existingJSON, err := json.Marshal(existing)
	require.NoError(t, err)

	cases := []struct {
		name       string
		body       string
		wantNilCfg bool
		wantChunk  types.ChunkingConfig
	}{
		{
			name:       "config omitted keeps chunking untouched",
			body:       `{"name":"n"}`,
			wantNilCfg: true,
		},
		{
			name:      "config without chunking_config keeps stored values",
			body:      `{"name":"n","config":{"summary_model_id":"m1"}}`,
			wantChunk: existing,
		},
		{
			name:      "explicit chunking_config replaces",
			body:      `{"name":"n","config":{"chunking_config":{"chunk_size":256,"chunk_overlap":0,"separators":["==="]}}}`,
			wantChunk: types.ChunkingConfig{ChunkSize: 256, Separators: []string{"==="}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 1, ChunkingConfig: existing}
			svc := &chunkingPutKBService{kb: kb}
			shares := &vlmUpdateShareStub{permission: types.OrgRoleAdmin}
			h := &KnowledgeBaseHandler{service: svc, kbShareService: shares}
			enabled := true
			r := gin.New()
			r.Use(middleware.ErrorHandler(), func(c *gin.Context) {
				ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(1))
				ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleAdmin)
				c.Request = c.Request.WithContext(ctx)
				c.Set(types.TenantIDContextKey.String(), uint64(1))
				c.Next()
			})
			r.PUT("/:id", middleware.RequireKBAccess(
				middleware.KBIDFromParam("id"), types.OrgRoleEditor, svc, shares, nil,
				&config.Config{Tenant: &config.TenantConfig{EnableRBAC: &enabled}},
			), h.UpdateKnowledgeBase)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/kb-1", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			if tc.wantNilCfg {
				require.Nil(t, svc.gotConfig)
				return
			}
			require.NotNil(t, svc.gotConfig)
			if tc.name == "config without chunking_config keeps stored values" {
				// Marshaling the value type always carries every field, so
				// compare against the stored JSON body.
				require.JSONEq(t, string(existingJSON), mustJSON(t, svc.gotConfig.ChunkingConfig))
			} else {
				require.JSONEq(t, mustJSON(t, tc.wantChunk), mustJSON(t, svc.gotConfig.ChunkingConfig))
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
