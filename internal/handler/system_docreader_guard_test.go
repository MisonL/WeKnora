package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ReconnectDocReader swaps the process-wide DocReader address, so every
// tenant's parsing traffic (including file bytes and parser credentials)
// follows it. The route is SystemAdmin-only; the handler repeats the check so
// moving the route back under a tenant-role group cannot silently widen it.
func TestReconnectDocReaderRequiresSystemAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name        string
		role        types.TenantRole
		systemAdmin bool
		wantStatus  int
	}{
		{name: "tenant admin denied", role: types.TenantRoleAdmin, wantStatus: http.StatusForbidden},
		{name: "tenant owner denied", role: types.TenantRoleOwner, wantStatus: http.StatusForbidden},
		{name: "system admin passes handler check", role: types.TenantRoleViewer, systemAdmin: true, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/system/docreader/reconnect",
				strings.NewReader(`{}`))
			c.Request.Header.Set("Content-Type", "application/json")
			ctx := context.WithValue(c.Request.Context(), types.TenantRoleContextKey, tc.role)
			ctx = context.WithValue(ctx, types.SystemAdminContextKey, tc.systemAdmin)
			c.Request = c.Request.WithContext(ctx)

			(&SystemHandler{}).ReconnectDocReader(c)

			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
		})
	}
}
