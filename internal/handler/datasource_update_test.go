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

type stubDSUpdateService struct {
	interfaces.DataSourceService
	ds        *types.DataSource
	gotUpdate *types.DataSource
}

func (s *stubDSUpdateService) GetDataSource(_ context.Context, id string) (*types.DataSource, error) {
	if s.ds == nil || s.ds.ID != id {
		return nil, http.ErrBodyNotAllowed
	}
	cp := *s.ds
	return &cp, nil
}

func (s *stubDSUpdateService) UpdateDataSource(_ context.Context, ds *types.DataSource) (*types.DataSource, error) {
	cp := *ds
	s.gotUpdate = &cp
	return &cp, nil
}

type stubDSKBService struct {
	interfaces.KnowledgeBaseService
	kb *types.KnowledgeBase
}

func (s *stubDSKBService) GetKnowledgeBaseByID(_ context.Context, id string) (*types.KnowledgeBase, error) {
	if s.kb == nil || s.kb.ID != id {
		return nil, http.ErrBodyNotAllowed
	}
	cp := *s.kb
	return &cp, nil
}

// A PUT that only renames a data source must not wipe sync_schedule,
// sync_deletions or the other stored settings back to zero values.
func TestUpdateDataSourcePreservesOmittedFields(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantCode      int
		wantName      string
		wantSchedule  string
		wantDeletions bool
		wantSyncMode  string
		wantConflict  string
	}{
		{
			name:          "rename only keeps sync settings",
			body:          `{"name":"new name"}`,
			wantCode:      http.StatusOK,
			wantName:      "new name",
			wantSchedule:  "0 0 9 * * *",
			wantDeletions: false,
			wantSyncMode:  "incremental",
			wantConflict:  "skip",
		},
		{
			name:          "explicit sync fields still apply",
			body:          `{"name":"renamed","sync_schedule":"","sync_deletions":true}`,
			wantCode:      http.StatusOK,
			wantName:      "renamed",
			wantSchedule:  "",
			wantDeletions: true,
			wantSyncMode:  "incremental",
			wantConflict:  "skip",
		},
		{
			name:          "sync_deletions true without schedule",
			body:          `{"sync_deletions":true}`,
			wantCode:      http.StatusOK,
			wantName:      "Existing",
			wantSchedule:  "0 0 9 * * *",
			wantDeletions: true,
			wantSyncMode:  "incremental",
			wantConflict:  "skip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := &types.DataSource{
				ID:               "ds-1",
				TenantID:         1,
				KnowledgeBaseID:  "kb-1",
				Name:             "Existing",
				Type:             "notion",
				SyncSchedule:     "0 0 9 * * *",
				SyncMode:         "incremental",
				Status:           "active",
				ConflictStrategy: "skip",
				SyncDeletions:    false,
			}
			svc := &stubDSUpdateService{ds: ds}
			h := &DataSourceHandler{
				service:   svc,
				kbService: &stubDSKBService{kb: &types.KnowledgeBase{ID: "kb-1", TenantID: 1}},
			}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(1))
				c.Request = c.Request.WithContext(ctx)
				c.Set(types.TenantIDContextKey.String(), uint64(1))
				c.Next()
			})
			r.PUT("/datasource/:id", h.UpdateDataSource)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/datasource/ds-1", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			require.NotNil(t, svc.gotUpdate)
			got := svc.gotUpdate
			require.Equal(t, tc.wantName, got.Name)
			require.Equal(t, tc.wantSchedule, got.SyncSchedule)
			require.Equal(t, tc.wantDeletions, got.SyncDeletions)
			require.Equal(t, tc.wantSyncMode, got.SyncMode)
			require.Equal(t, tc.wantConflict, got.ConflictStrategy)
			require.Equal(t, "active", got.Status, "status must not be wiped")
			require.Equal(t, "kb-1", got.KnowledgeBaseID, "knowledge base must not change")
		})
	}
}
