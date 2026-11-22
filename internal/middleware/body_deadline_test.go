package middleware

import (
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newDeadlineTestServer starts a real server (httptest) so the ResponseController
// resolves to a connection that actually supports SetReadDeadline.
func newDeadlineTestServer(t *testing.T, budget func(int64) time.Duration, handler gin.HandlerFunc) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestBodyDeadlineWith(budget))
	r.POST("/body", handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestStalledBodyDoesNotHoldTheHandlerForever: a client sends the headers and
// part of the body, then stops. The handler's body read must fail at the
// deadline instead of blocking until the client gives up.
func TestStalledBodyDoesNotHoldTheHandlerForever(t *testing.T) {
	readErr := make(chan error, 1)
	srv := newDeadlineTestServer(t, func(int64) time.Duration { return 200 * time.Millisecond },
		func(c *gin.Context) {
			_, err := io.ReadAll(c.Request.Body)
			readErr <- err
			if err != nil {
				c.Status(http.StatusBadRequest)
				return
			}
			c.Status(http.StatusOK)
		})

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	// Declare 1000 bytes, send 10, then stall.
	_, err = io.WriteString(conn, "POST /body HTTP/1.1\r\nHost: test\r\nContent-Length: 1000\r\n\r\n0123456789")
	require.NoError(t, err)

	select {
	case err := <-readErr:
		require.Error(t, err, "the stalled body read must fail at the deadline")
		var ne net.Error
		require.ErrorAs(t, err, &ne)
		require.True(t, ne.Timeout(), "the read must fail because of the deadline, not a reset: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("handler was still blocked on the stalled body after the deadline")
	}
}

// TestDeadlineIsClearedOnceTheBodyIsRead: handlers that read a small body and
// then run long (chat streaming, imports) must not be cut off by the read
// deadline.
func TestDeadlineIsClearedOnceTheBodyIsRead(t *testing.T) {
	srv := newDeadlineTestServer(t, func(int64) time.Duration { return 150 * time.Millisecond },
		func(c *gin.Context) {
			_, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			// Run well past the read deadline, as a streamed answer would.
			time.Sleep(3 * 150 * time.Millisecond)
			c.String(http.StatusOK, "done")
		})

	start := time.Now()
	resp, err := http.Post(srv.URL+"/body", "text/plain", strings.NewReader("hi"))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "done", string(body))
	require.GreaterOrEqual(t, time.Since(start), 450*time.Millisecond,
		"the handler really did run past the read deadline")
}

// TestBodyReadBudget: the budget scales with the declared size at the floor
// rate and is capped.
func TestBodyReadBudget(t *testing.T) {
	require.Equal(t, bodyReadCap, bodyReadBudget(0), "unknown length gets only the cap")
	require.Equal(t, bodyReadBaseGrace, bodyReadBudget(1), "a tiny body gets the base grace")
	// 32 KiB/s: 3 MiB is about 96 seconds above the grace.
	require.Equal(t, bodyReadBaseGrace+96*time.Second, bodyReadBudget(3*1024*1024))
	require.Equal(t, bodyReadCap, bodyReadBudget(64*1024*1024*1024), "a huge body is capped")
	// Content-Length is attacker-controlled; the largest int64 must still cap
	// rather than overflow into a past deadline.
	require.Equal(t, bodyReadCap, bodyReadBudget(math.MaxInt64), "the budget must not overflow")
	require.Greater(t, bodyReadBudget(math.MaxInt64), time.Duration(0))
}
