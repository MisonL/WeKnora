package wecom

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/im"
)

type failingTransport func(*http.Request) (*http.Response, error)

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func swapHTTPClient(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	previous := httpClient
	httpClient = &http.Client{Transport: transport}
	t.Cleanup(func() { httpClient = previous })
}

// gettoken puts corpsecret in the query string. A transport failure surfaces a
// *url.Error carrying the full URL, and that error reaches logger.Errorf.
func TestGetAccessTokenErrorRedactsCorpSecret(t *testing.T) {
	const secret = "placeholder-corp-secret"
	swapHTTPClient(t, failingTransport(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("dial refused")}
	}))

	adapter := &WebhookAdapter{corpID: "corp-1", agentSecret: secret, apiBaseURL: defaultAPIBaseURL}
	_, err := adapter.getAccessToken(context.Background())
	if err == nil {
		t.Fatal("expected getAccessToken to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked corpsecret: %v", err)
	}
	if !strings.Contains(err.Error(), "corpsecret=%2A%2A%2A") {
		t.Fatalf("error no longer shows a redacted corpsecret parameter: %v", err)
	}
}

// message/send carries the access token in the query string; the same url.Error
// wrapper used to copy it into the log line.
func TestSendToUserErrorRedactsAccessToken(t *testing.T) {
	const token = "placeholder-token"
	swapHTTPClient(t, failingTransport(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("connection reset")}
	}))

	adapter := &WebhookAdapter{apiBaseURL: defaultAPIBaseURL, corpAgentID: 1}
	err := adapter.sendToUser(context.Background(), token, "user-1", &im.ReplyMessage{Content: "hi"})
	if err == nil {
		t.Fatal("expected sendToUser to fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked access_token: %v", err)
	}
}
