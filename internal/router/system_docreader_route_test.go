package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// POST /system/docreader/reconnect repoints the process-wide DocReader used by
// every tenant. It must sit behind the SystemAdmin guard, not the workspace
// Admin ladder.
func TestDocReaderReconnectRouteRequiresSystemAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enforce := true
	g := &rbacGuards{cfg: &config.Config{Tenant: &config.TenantConfig{EnableRBAC: &enforce}}}

	var role types.TenantRole
	var systemAdmin bool
	engine := gin.New()
	engine.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	engine.Use(func(c *gin.Context) {
		ctx := types.WithCaller(c.Request.Context(), types.Caller{TenantID: 1, UserID: "probe", Role: role})
		ctx = context.WithValue(ctx, types.SystemAdminContextKey, systemAdmin)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	RegisterSystemRoutes(v1, &handler.SystemHandler{}, g)
	RegisterSystemAdminRoutes(v1, &handler.SystemHandler{}, nil, g)

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/docreader/reconnect",
			strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	role, systemAdmin = types.TenantRoleAdmin, false
	require.Equal(t, http.StatusForbidden, post().Code,
		"a workspace Admin must not be able to repoint the shared DocReader")

	role, systemAdmin = types.TenantRoleAdmin, true
	require.NotEqual(t, http.StatusForbidden, post().Code,
		"a system administrator must pass the route guard")
}
