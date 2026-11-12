package openaicompletions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A caller that retries the handshake itself marks the context so the
// transport skips its own budget. Without the marker both layers retry and a
// sustained outage issues maxStreamStartRetries x streamStartRetries
// handshakes for one user request.
func TestChatStream_TransportRetryIsScopedByContext(t *testing.T) {
	cases := []struct {
		name     string
		ctx      context.Context
		wantReqs int32
	}{
		{"default keeps the transport budget", context.Background(), streamStartRetries + 1},
		{"caller owns the budget", api.WithoutTransportRetry(context.Background()), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSRF_WHITELIST", "127.0.0.1")
			var requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&requests, 1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()

			c := New(Config{
				Endpoint: api.Endpoint{BaseURL: server.URL + "/v1", Model: "m", Auth: api.BearerAuth("sk")},
				Settings: api.DefaultOpenAICompletions(),
			})
			_, err := c.ChatStream(tc.ctx, []api.Message{{Role: "user", Content: "hi"}}, nil)
			require.Error(t, err)
			assert.Equal(t, tc.wantReqs, atomic.LoadInt32(&requests))
		})
	}
}
