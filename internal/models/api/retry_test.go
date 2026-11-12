package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastPolicy() RetryPolicy { return RetryPolicy{MaxRetries: 3} }

// The failure this helper exists to make impossible: every attempt fails at
// the transport, and the caller gets an error rather than a nil response with
// a nil error to check. The eight hand-written loops it replaces returned
// (nil, nil) here whenever a `:=` shadowed the enclosing error.
func TestPostJSONWithRetryAlwaysReportsAnUnreachableUpstream(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	var out map[string]any
	err := Endpoint{}.PostJSONWithRetry(
		context.Background(), url, map[string]any{"a": 1}, &out, fastPolicy(), "test",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test")
}

func TestPostJSONWithRetrySucceedsAfterATransportFailure(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Clone())
		attempts++
		if attempts == 1 {
			// Hang up mid-request so the client sees a transport error.
			hijacker, ok := w.(http.Hijacker)
			require.True(t, ok)
			conn, _, err := hijacker.Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var out map[string]any
	err := Endpoint{}.PostJSONWithRetry(
		types.WithSessionID(context.Background(), "retry-session"),
		server.URL, map[string]any{}, &out, fastPolicy(), "test",
	)
	require.NoError(t, err)
	assert.Equal(t, true, out["ok"])
	assert.Equal(t, 2, attempts)
	for _, h := range headers {
		assert.Equal(t, "retry-session", h.Get("X-Conversation-ID"))
		assert.Equal(t, "WeKnora", h.Get("User-Agent"))
	}
}

// A non-2xx reply is the vendor answering. Repeating a rejected request costs
// quota, delays the error, and cannot change the answer.
func TestPostJSONWithRetryDoesNotRetryAVendorRejection(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"input too long"}`))
	}))
	defer server.Close()

	err := Endpoint{}.PostJSONWithRetry(
		context.Background(), server.URL, map[string]any{}, nil, fastPolicy(), "test",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input too long")
	assert.Equal(t, 1, attempts, "a rejection must not be repeated")
}

// A reply that arrived but does not decode is not a transport failure: the
// vendor did the work, and sending the request again bills it again.
func TestPostJSONWithRetryDoesNotRetryAnUndecodableReply(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	var out map[string]any
	err := Endpoint{}.PostJSONWithRetry(
		context.Background(), server.URL, map[string]any{}, &out, fastPolicy(), "test",
	)
	require.Error(t, err)
	assert.Equal(t, 1, attempts)
}

func TestPostJSONWithRetryStopsWhenTheContextIsDone(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Endpoint{}.PostJSONWithRetry(
		ctx, url, map[string]any{}, nil,
		RetryPolicy{MaxRetries: 3, BaseDelay: time.Minute}, "test",
	)
	require.Error(t, err)
}

func TestRetryPolicyBackoffDoublesAndIsCapped(t *testing.T) {
	p := RetryPolicy{MaxRetries: 5, BaseDelay: time.Second, MaxDelay: 4 * time.Second}
	assert.Equal(t, time.Second, p.delay(1))
	assert.Equal(t, 2*time.Second, p.delay(2))
	assert.Equal(t, 4*time.Second, p.delay(3))
	assert.Equal(t, 4*time.Second, p.delay(4), "capped")
	assert.Zero(t, RetryPolicy{MaxRetries: 2}.delay(1), "no base delay means no waiting")
}

// A reply cut off mid-body never arrived, so it is sent again.
func TestPostJSONWithRetryRetriesAReplyCutOffWhileReading(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			// Promise more than is sent, then hang up.
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"a":`))
			w.(http.Flusher).Flush() // the status and half a body are on the wire
			hj, ok := w.(http.Hijacker)
			require.True(t, ok)
			conn, _, err := hj.Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer server.Close()

	var out map[string]any
	err := Endpoint{}.PostJSONWithRetry(
		context.Background(), server.URL, map[string]any{}, &out, fastPolicy(), "test",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, float64(1), out["a"])
}

// The phase that failed is named, so the log says whether the request or the
// reply was lost.
func TestReadFailureIsReportedAsSuch(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"a":`))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		_ = conn.Close()
	}))
	defer server.Close()

	err := Endpoint{}.PostJSON(context.Background(), server.URL, map[string]any{}, &map[string]any{})
	var transportErr *TransportError
	require.ErrorAs(t, err, &transportErr)
	assert.Equal(t, "read response", transportErr.Op)
}

// 429 and the 5xx family mean "the backend was unavailable", so sending the
// request again is exactly what the vendor asked for. Before this, every
// non-2xx was final and a rate-limited embedding call failed outright.
func TestPostJSONWithRetryRetriesRateLimitAndServerErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("SSRF_WHITELIST", "127.0.0.1")
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if attempts < 3 {
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()

			var out map[string]any
			err := Endpoint{}.PostJSONWithRetry(
				context.Background(), server.URL, map[string]any{}, &out, fastPolicy(), "test",
			)
			require.NoError(t, err)
			assert.Equal(t, true, out["ok"])
			assert.Equal(t, 3, attempts, "a retryable status must be retried until it succeeds")
		})
	}
}

// A 4xx that is not a rate limit is the vendor rejecting the request body:
// repeating it cannot change the answer, so it stays final.
func TestPostJSONWithRetryKeepsOther4xxFinal(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("SSRF_WHITELIST", "127.0.0.1")
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(status)
			}))
			defer server.Close()

			err := Endpoint{}.PostJSONWithRetry(
				context.Background(), server.URL, map[string]any{}, nil, fastPolicy(), "test",
			)
			require.Error(t, err)
			assert.Equal(t, 1, attempts)
		})
	}
}

// Retry-After is the vendor telling us when to come back. Honouring it beats
// guessing with our own backoff.
func TestRetryPolicyHonoursRetryAfter(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	hint := &HTTPError{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"2"}}}

	wait := p.retryWait(1, hint)
	assert.GreaterOrEqual(t, wait, time.Second, "the vendor hint must not be shortened by our backoff")
	assert.LessOrEqual(t, wait, maxRetryAfterHint, "an absurd hint must be capped")
}

// Jitter keeps a batch of workers from retrying in lockstep; it must stay
// within the top half of the intended window.
func TestRetryWaitStaysWithinItsWindow(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second}
	for attempt := 1; attempt <= 3; attempt++ {
		base := p.delay(attempt)
		for i := 0; i < 50; i++ {
			got := p.retryWait(attempt, nil)
			assert.GreaterOrEqual(t, got, base/2)
			assert.LessOrEqual(t, got, base)
		}
	}
}
