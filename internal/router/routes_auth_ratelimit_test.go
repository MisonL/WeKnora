package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// authRateLimitStub implements just enough of UserService for the
// rate-limited auth routes; the limiter rejects before these run.
type authRateLimitStub struct {
	interfaces.UserService
}

func (s *authRateLimitStub) Login(context.Context, *types.LoginRequest) (*types.LoginResponse, error) {
	return &types.LoginResponse{Success: false, Message: "stub"}, nil
}

func (s *authRateLimitStub) RefreshToken(context.Context, string) (string, string, error) {
	return "", "", context.Canceled
}

func newAuthRateLimitRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := handler.NewAuthHandler(
		&config.Config{Auth: &config.AuthConfig{}}, &authRateLimitStub{}, nil, nil, nil)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	RegisterAuthRoutes(r.Group("/api/v1"), h, &rbacGuards{})
	return r
}

func postAsIP(r *gin.Engine, path, ip string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":4321"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestLoginIsRateLimitedPerIP(t *testing.T) {
	r := newAuthRateLimitRouter()
	// The limiter is a process-wide singleton; use unique test-net IPs so
	// repeated runs (-count=N) don't inherit a previous run's budget.
	suffix := int(time.Now().UnixNano()%200) + 1
	loginIP := fmt.Sprintf("203.0.113.%d", suffix)

	// 30 requests per IP per minute; the 31st must be throttled.
	for i := 0; i < 30; i++ {
		if code := postAsIP(r, "/api/v1/auth/login", loginIP); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was throttled too early", i+1)
		}
	}
	if code := postAsIP(r, "/api/v1/auth/login", loginIP); code != http.StatusTooManyRequests {
		t.Fatalf("login request 31 status = %d, want 429", code)
	}
}

// Refresh has its own, larger bucket. Sharing login's meant a busy NAT could
// exhaust it with login attempts and then sign every one of its users out, as
// the SPA treats a failed refresh as a logout.
func TestRefreshHasItsOwnLargerBudget(t *testing.T) {
	r := newAuthRateLimitRouter()
	suffix := int(time.Now().UnixNano()%200) + 1
	ip := fmt.Sprintf("198.51.100.%d", suffix)

	// Exhaust the login bucket for this IP first.
	for i := 0; i < 30; i++ {
		_ = postAsIP(r, "/api/v1/auth/login", ip)
	}
	if code := postAsIP(r, "/api/v1/auth/login", ip); code != http.StatusTooManyRequests {
		t.Fatalf("login request 31 status = %d, want 429", code)
	}
	// Refresh is unaffected by that exhaustion.
	if code := postAsIP(r, "/api/v1/auth/refresh", ip); code == http.StatusTooManyRequests {
		t.Fatal("exhausting the login bucket must not throttle refresh")
	}
	// Its own budget is larger than login's 30/min.
	for i := 0; i < 100; i++ {
		if code := postAsIP(r, "/api/v1/auth/refresh", ip); code == http.StatusTooManyRequests {
			t.Fatalf("refresh request %d was throttled too early", i+2)
		}
	}
}
