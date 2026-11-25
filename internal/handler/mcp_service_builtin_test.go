package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// builtinMCPServiceStub records the rows the handler hands to the service.
type builtinMCPServiceStub struct {
	interfaces.MCPServiceService
	created *types.MCPService
	updated *types.MCPService
}

func (s *builtinMCPServiceStub) CreateMCPService(_ context.Context, service *types.MCPService) error {
	s.created = service
	return nil
}

func (s *builtinMCPServiceStub) UpdateMCPService(
	_ context.Context, service *types.MCPService, _ map[string]bool,
) error {
	s.updated = service
	return nil
}

func (s *builtinMCPServiceStub) GetMCPServiceByID(
	_ context.Context, _ uint64, id string,
) (*types.MCPService, error) {
	return &types.MCPService{ID: id, TenantID: 7}, nil
}

func (s *builtinMCPServiceStub) ListMCPMetadataSummaries(
	context.Context, uint64, []*types.MCPService,
) (map[string]*types.MCPMetadataSummary, error) {
	return nil, nil
}

// A workspace Admin must not be able to mint a globally visible builtin MCP
// service by putting is_builtin into the create body: builtin rows are
// returned to every workspace's agent catalog.
func TestCreateMCPServiceIgnoresClientBuiltinFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &builtinMCPServiceStub{}
	router := gin.New()
	router.Use(middleware.ErrorHandler(), func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
	})
	h := &MCPServiceHandler{mcpServiceService: service}
	router.POST("/mcp-services", h.CreateMCPService)

	body := `{"name":"Service","transport_type":"sse","is_builtin":true`
	// URL is intentionally omitted: the handler validates outbound URLs and
	// an SSRF probe would make this test depend on DNS.
	body += `}`
	request := httptest.NewRequest(http.MethodPost, "/mcp-services", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, service.created)
	require.False(t, service.created.IsBuiltin,
		"workspace-created MCP services must never be globally visible")
}

// Update must also ignore is_builtin: an Admin can neither promote a service
// to builtin nor keep a stale true flag alive through a partial update.
func TestUpdateMCPServiceIgnoresClientBuiltinFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &builtinMCPServiceStub{}
	router := gin.New()
	router.Use(middleware.ErrorHandler(), func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
	})
	h := &MCPServiceHandler{mcpServiceService: service}
	router.PUT("/mcp-services/:id", h.UpdateMCPService)

	body := `{"name":"Renamed","is_builtin":true}`
	request := httptest.NewRequest(http.MethodPut, "/mcp-services/svc-1", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, service.updated)
	require.False(t, service.updated.IsBuiltin,
		"a workspace update must not set the globally visible builtin flag")
}
