package utils

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURLQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wecom corpsecret",
			in:   "https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=corp-1&corpsecret=S3CRET",
			want: "https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=corp-1&corpsecret=%2A%2A%2A",
		},
		{
			name: "access token",
			in:   "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=S3CRET&debug=1",
			want: "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=%2A%2A%2A&debug=1",
		},
		{
			name: "dingtalk session webhook",
			in:   "https://oapi.dingtalk.com/robot/sendBySession?session=S3CRET",
			want: "https://oapi.dingtalk.com/robot/sendBySession?session=%2A%2A%2A",
		},
		{
			name: "harmless query is untouched",
			in:   "https://example.com/path?page=2&q=hello",
			want: "https://example.com/path?page=2&q=hello",
		},
		{
			name: "no query",
			in:   "https://example.com/path",
			want: "https://example.com/path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactURLQuery(tc.in); got != tc.want {
				t.Fatalf("RedactURLQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := RedactURLQuery("https://example.com/path?corpsecret=S3CRET"); strings.Contains(got, "S3CRET") {
		t.Fatalf("RedactURLQuery leaked the secret: %q", got)
	}
}

func TestRedactRequestErrorKeepsWrappingContext(t *testing.T) {
	inner := &url.Error{
		Op:  "Get",
		URL: "https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=corp-1&corpsecret=S3CRET",
		Err: errors.New("dial refused"),
	}
	// Call sites redact the raw transport error, then wrap it; fmt.Errorf
	// renders its message immediately, so redacting after wrapping cannot
	// rewrite the outer text.
	err := fmt.Errorf("request access token: %w", RedactRequestError(inner))

	if !strings.Contains(err.Error(), "request access token:") {
		t.Fatalf("wrapping context was lost: %v", err)
	}
	if strings.Contains(err.Error(), "S3CRET") {
		t.Fatalf("error leaked the query secret: %v", err)
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		t.Fatalf("error no longer unwraps to *url.Error: %v", err)
	}
}

func TestRedactRequestErrorPassesThroughOtherErrors(t *testing.T) {
	err := errors.New("plain failure")
	if got := RedactRequestError(err); got != err {
		t.Fatalf("RedactRequestError(%v) = %v, want the same error", err, got)
	}
}

func TestSummarizeUpstreamBodyKeepsOnlyErrcodeAndMessage(t *testing.T) {
	body := []byte(`{"errcode":40013,"errmsg":"invalid corpid","request":{"corpsecret":"S3CRET","debug":"S3CRET"}}`)
	got := SummarizeUpstreamBody(body)
	if strings.Contains(got, "S3CRET") {
		t.Fatalf("summary leaked the raw body: %q", got)
	}
	if !strings.Contains(got, "errcode=40013") || !strings.Contains(got, "invalid corpid") {
		t.Fatalf("summary lost errcode/errmsg: %q", got)
	}
	if !strings.Contains(got, "body_len=") {
		t.Fatalf("summary lost the body length: %q", got)
	}
}

func TestSummarizeUpstreamBodyDingTalkShape(t *testing.T) {
	body := []byte(`{"code":"InvalidAuthentication","message":"bad credentials","appSecret":"S3CRET"}`)
	got := SummarizeUpstreamBody(body)
	if strings.Contains(got, "S3CRET") {
		t.Fatalf("summary leaked the raw body: %q", got)
	}
	if !strings.Contains(got, "InvalidAuthentication") || !strings.Contains(got, "bad credentials") {
		t.Fatalf("summary lost code/message: %q", got)
	}
}

func TestSummarizeUpstreamBodyNonJSONKeepsLengthOnly(t *testing.T) {
	got := SummarizeUpstreamBody([]byte("<html>proxy error S3CRET</html>"))
	if strings.Contains(got, "S3CRET") || strings.Contains(got, "<html>") {
		t.Fatalf("summary leaked a non-JSON body: %q", got)
	}
	if !strings.Contains(got, "body_len=") {
		t.Fatalf("summary lost the body length: %q", got)
	}
}

// Presigned download URLs carry the HMAC signature and its expiry in the
// query string; both must be masked before the access logger copies the URL.
func TestSanitizeQueryRedactsPresignedParameters(t *testing.T) {
	got := SanitizeQuery("file_path=local%3A%2F%2F1%2Fa.png&tenant_id=1&expires=1700000000&sig=deadbeef01&signature=deadbeef02")
	for _, leaked := range []string{"deadbeef01", "deadbeef02", "1700000000"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("SanitizeQuery() leaked %q: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "file_path=") || !strings.Contains(got, "tenant_id=1") {
		t.Fatalf("SanitizeQuery() dropped non-sensitive parameters: %q", got)
	}
	if !strings.Contains(got, "sig=%2A%2A%2A") || !strings.Contains(got, "signature=%2A%2A%2A") {
		t.Fatalf("SanitizeQuery() did not mask sig/signature: %q", got)
	}
}
