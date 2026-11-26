package dingtalk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

type stubTransport func(*http.Request) (*http.Response, error)

func (f stubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func swapDingTalkHTTPClient(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	previous := httpClient
	httpClient = &http.Client{Transport: transport}
	t.Cleanup(func() { httpClient = previous })
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// The accessToken endpoint used to return the raw upstream body in its error,
// and a proxy/gateway error page can echo the request (appKey/appSecret) back.
func TestAccessTokenErrorOmitsUpstreamBodySecret(t *testing.T) {
	const secret = "APP-SECRET-placeholder-credential"
	swapDingTalkHTTPClient(t, stubTransport(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"code":"InvalidAuthentication","message":"bad credentials","request":{"appSecret":"`+secret+`"}}`), nil
	}))

	adapter := &Adapter{clientID: "app-key", clientSecret: secret}
	_, err := adapter.getAccessToken(context.Background())
	if err == nil {
		t.Fatal("expected getAccessToken to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("error leaked the upstream body: %v", err)
	}
	if !strings.Contains(msg, "InvalidAuthentication") {
		t.Fatalf("error should keep errcode/message: %v", err)
	}
	if !strings.Contains(msg, "body_len=") {
		t.Fatalf("error should report the body length: %v", err)
	}
}

// The session webhook URL itself is a credential (session parameter). A
// transport error mirrors the full URL into *url.Error.
func TestSessionWebhookErrorRedactsSessionCredential(t *testing.T) {
	secutils.SetSSRFWhitelistFromRaw("oapi.dingtalk.com")
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)

	const secret = "SESSION-placeholder-credential"
	target := "https://oapi.dingtalk.com/robot/sendBySession?session=" + secret
	swapDingTalkHTTPClient(t, stubTransport(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("timeout")}
	}))

	err := (&Adapter{}).SendReply(context.Background(),
		&im.IncomingMessage{Extra: map[string]string{"session_webhook": target}},
		&im.ReplyMessage{Content: "hello"})
	if err == nil {
		t.Fatal("expected SendReply to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the session webhook credential: %v", err)
	}
}

func TestDingTalkAPIErrorSummarizesUpstreamBody(t *testing.T) {
	const secret = "placeholder-upstream"
	swapDingTalkHTTPClient(t, stubTransport(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError,
			`{"code":"InternalError","message":"boom","debug":"`+secret+`"}`), nil
	}))

	adapter := &Adapter{clientID: "app-key", clientSecret: "app-secret"}
	adapter.token = "cached-token"
	adapter.tokenExpAt = time.Now().Add(time.Hour)

	_, err := adapter.dingtalkAPI(context.Background(), http.MethodPost, "/v1.0/card/instances/createAndDeliver", map[string]string{})
	if err == nil {
		t.Fatal("expected dingtalkAPI to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("error leaked the raw upstream body: %v", err)
	}
	if !strings.Contains(msg, "body_len=") {
		t.Fatalf("error should report the body length: %v", msg)
	}
}
