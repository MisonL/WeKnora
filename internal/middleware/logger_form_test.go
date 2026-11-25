package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newBodyContext(t *testing.T, method, path, contentType, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)
	return c
}

// mattermost and other IM webhooks post form-urlencoded bodies; the plain
// JSON redactor does not see key=value pairs, so the form path needs its own
// parse-and-mask step.
func TestReadRequestBodyRedactsFormEncodedSecrets(t *testing.T) {
	payload := "token=form-secret-token&channel_id=general&api_key=form-api-key&page=2"
	c := newBodyContext(t, http.MethodPost, "/api/v1/im/webhook/mattermost",
		"application/x-www-form-urlencoded", payload)

	got := readRequestBody(c)
	for _, leaked := range []string{"form-secret-token", "form-api-key"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("form body leaked %q: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "channel_id=general") || !strings.Contains(got, "page=2") {
		t.Fatalf("non-sensitive form fields were lost: %q", got)
	}
	if !strings.Contains(got, "token=%2A%2A%2A") || !strings.Contains(got, "api_key=%2A%2A%2A") {
		t.Fatalf("sensitive form fields were not masked: %q", got)
	}

	// The handler must still see the untouched request body.
	rest, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if string(rest) != payload {
		t.Fatalf("handler saw a rewritten body: %q", rest)
	}
}

// /me/env-vars carries a personal secret under the generic key "value"; the
// logger only treats it as sensitive on that path, so other resources that
// legitimately use "value" stay readable.
func TestReadRequestBodyRedactsEnvVarValue(t *testing.T) {
	payload := `{"skill_id":"skill-1","name":"GITHUB_TOKEN","value":"env-var-secret","keep":"visible"}`
	c := newBodyContext(t, http.MethodPut, "/api/v1/me/env-vars/skill", "application/json", payload)

	got := readRequestBody(c)
	if strings.Contains(got, "env-var-secret") {
		t.Fatalf("env var body leaked the value: %q", got)
	}
	if !strings.Contains(got, `"value":"***"`) {
		t.Fatalf("env var value was not masked: %q", got)
	}
	if !strings.Contains(got, `"keep":"visible"`) || !strings.Contains(got, "GITHUB_TOKEN") {
		t.Fatalf("non-sensitive fields were redacted: %q", got)
	}
}

func TestReadRequestBodyKeepsValueAtOtherPaths(t *testing.T) {
	payload := `{"value":"ordinary-value"}`
	c := newBodyContext(t, http.MethodPut, "/api/v1/custom-fields/1", "application/json", payload)

	got := readRequestBody(c)
	if !strings.Contains(got, "ordinary-value") {
		t.Fatalf("value outside /me/env-vars must stay readable: %q", got)
	}
}

func TestReadRequestBodyRedactsEnvVarValueInFormBody(t *testing.T) {
	c := newBodyContext(t, http.MethodPut, "/api/v1/me/env-vars/sandbox",
		"application/x-www-form-urlencoded", "name=SLACK_BOT_TOKEN&value=form-env-secret")

	got := readRequestBody(c)
	if strings.Contains(got, "form-env-secret") {
		t.Fatalf("form env var body leaked the value: %q", got)
	}
	if !strings.Contains(got, "name=SLACK_BOT_TOKEN") {
		t.Fatalf("non-sensitive form field was lost: %q", got)
	}
}

// A truncated or otherwise invalid JSON body on the env-var surface falls back
// to the regex redactor, which must still know about the path-specific key.
func TestReadRequestBodyRedactsEnvVarValueInFallbackBody(t *testing.T) {
	payload := `{"name":"GITHUB_TOKEN","value":"fallback-env-secret"`
	c := newBodyContext(t, http.MethodPut, "/api/v1/me/env-vars/skill", "application/json", payload)

	got := readRequestBody(c)
	if strings.Contains(got, "fallback-env-secret") {
		t.Fatalf("fallback redactor leaked the env var value: %q", got)
	}
}
