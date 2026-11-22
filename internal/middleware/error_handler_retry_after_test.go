package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
)

// A 429 that names a wait must say so in the header: a client that only sees
// the status has to guess, and a guessed backoff is how a full server gets
// hammered by the retries meant to relieve it.
func TestErrorHandlerSendsRetryAfterWhenTheErrorStatesOne(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ErrorHandler())
	router.GET("/busy", func(c *gin.Context) {
		_ = c.Error(apperrors.NewTooManyRequestsError("busy").WithRetryAfter(7))
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/busy", nil))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "7", rec.Header().Get("Retry-After"))
}

// A 429 with no estimate is still a valid 429: the header is left off rather
// than filled with a made-up number.
func TestErrorHandlerOmitsRetryAfterWithoutAnEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ErrorHandler())
	router.GET("/busy", func(c *gin.Context) {
		_ = c.Error(apperrors.NewTooManyRequestsError("busy"))
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/busy", nil))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"))
}
