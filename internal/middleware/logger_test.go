package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

func TestSanitizeBody(t *testing.T) {
	// JSON bodies go through the structured redactor, which re-marshals the
	// document (map keys are ordered), so those cases are compared
	// semantically. Anything that is not valid JSON falls back to the regex
	// path and is compared byte for byte.
	assertSanitized := func(t *testing.T, in, want string) {
		t.Helper()
		got := sanitizeBody(in)
		if json.Valid([]byte(in)) {
			if !json.Valid([]byte(got)) {
				t.Fatalf("sanitizeBody(%q) produced invalid JSON: %s", in, got)
			}
			var gotV, wantV any
			if err := json.Unmarshal([]byte(got), &gotV); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			if err := json.Unmarshal([]byte(want), &wantV); err != nil {
				t.Fatalf("unmarshal want: %v", err)
			}
			gb, _ := json.Marshal(gotV)
			wb, _ := json.Marshal(wantV)
			if string(gb) != string(wb) {
				t.Errorf("sanitizeBody(%q)\n got: %s\nwant: %s", in, got, want)
			}
			return
		}
		if got != want {
			t.Errorf("sanitizeBody(%q)\n got: %s\nwant: %s", in, got, want)
		}
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "browser device credentials",
			in: `{"pairing_link":"wss://example.com/#secret",` +
				`"next_token":"new-secret","deviceToken":"device-secret"}`,
			want: `{"pairing_link":"***","next_token":"***","deviceToken":"***"}`,
		},
		{
			name: "camelCase apiKey",
			in:   `{"modelName":"gpt-5.2","apiKey":"sk-secret-123","provider":"azure_openai"}`,
			want: `{"modelName":"gpt-5.2","apiKey":"***","provider":"azure_openai"}`,
		},
		{
			name: "snake_case api_key",
			in:   `{"api_key":"sk-secret-123"}`,
			want: `{"api_key":"***"}`,
		},
		{
			name: "PascalCase APIKey",
			in:   `{"APIKey":"sk-secret-123"}`,
			want: `{"APIKey":"***"}`,
		},
		{
			name: "secretKey camelCase",
			in:   `{"secretKey":"abc","accessKeyId":"id"}`,
			want: `{"secretKey":"***","accessKeyId":"***"}`,
		},
		{
			name: "refreshToken / accessToken camelCase",
			in:   `{"refreshToken":"rt","accessToken":"at"}`,
			want: `{"refreshToken":"***","accessToken":"***"}`,
		},
		{
			name: "password and token preserved as masked",
			in:   `{"password":"p","token":"t"}`,
			want: `{"password":"***","token":"***"}`,
		},
		{
			name: "sandbox terminal handshake ticket in JSON body",
			in:   `{"success":true,"data":{"ticket":"eyJhbGciOiJIUzI1NiJ9.payload.signature","expires_in":120}}`,
			want: `{"success":true,"data":{"ticket":"***","expires_in":120}}`,
		},
		{
			name: "snake_case new_password and old_password",
			in:   `{"email":"alice@example.com","new_password":"FreshPass9","old_password":"OldPass9"}`,
			want: `{"email":"alice@example.com","new_password":"***","old_password":"***"}`,
		},
		{
			name: "extra whitespace around colon",
			in:   `{"apiKey"  :   "leak"}`,
			want: `{"apiKey":"***"}`,
		},
		{
			name: "MCP OAuth JWT bearer assertion",
			in:   `{"assertion":"eyJhbGciOiJFUzI1NiJ9.payload.signature","redirect_uri":"https://example.com/cb"}`,
			want: `{"assertion":"***","redirect_uri":"https://example.com/cb"}`,
		},
		{
			name: "IM channel credentials", in: `{"platform":"wecom","credentials":{"bot_id":"aib-1","bot_secret":"s3","bot_name":"小科"},"name":"x"}`,
			want: `{"platform":"wecom","credentials":"***","name":"x"}`,
		},
		{
			name: "other IM platform secrets",
			in:   `{"app_secret":"a","app_token":"b","agent_secret":"c","encoding_aes_key":"d","encrypt_key":"e","verification_token":"f","signing_secret":"g","outgoing_token":"h","secret_token":"i","webhook_secret":"j"}`,
			want: `{"app_secret":"***","app_token":"***","agent_secret":"***","encoding_aes_key":"***","encrypt_key":"***","verification_token":"***","signing_secret":"***","outgoing_token":"***","secret_token":"***","webhook_secret":"***"}`,
		},
		{
			name: "non sensitive fields untouched",
			in:   `{"baseUrl":"https://example.com","modelName":"gpt"}`,
			want: `{"baseUrl":"https://example.com","modelName":"gpt"}`,
		},
		{
			name: "OAuth authorization response fields",
			in:   `{"authorization_url":"https://idp.example/authorize?state=secret","authorization_attempt":"secret-state"}`,
			want: `{"authorization_url":"***","authorization_attempt":"***"}`,
		},
		{
			name: "inline image data URI",
			in:   `{"query":"see chart","image":"data:image/png;base64,` + strings.Repeat("A", 80) + `"}`,
			want: `{"query":"see chart","image":"data:image/png;base64,[redacted 80 chars]"}`,
		},
		{
			name: "data URI inside a secret field is still masked",
			in:   `{"apiKey":"data:image/png;base64,AAAA"}`,
			want: `{"apiKey":"***"}`,
		},
		{
			name: "app_password",
			in:   `{"app_password":"S3CRET"}`,
			want: `{"app_password":"***"}`,
		},
		{
			name: "secret_access_key",
			in:   `{"secret_access_key":"S3CRET"}`,
			want: `{"secret_access_key":"***"}`,
		},
		{
			name: "access_key_id",
			in:   `{"access_key_id":"AKID-S3CRET"}`,
			want: `{"access_key_id":"***"}`,
		},
		{
			name: "camelCase accessKey",
			in:   `{"accessKey":"S3CRET"}`,
			want: `{"accessKey":"***"}`,
		},
		{
			name: "api_token",
			in:   `{"api_token":"S3CRET"}`,
			want: `{"api_token":"***"}`,
		},
		{
			name: "mineru keys and paddleocr token",
			in:   `{"mineru_api_key":"a","mineru_server_api_key":"b","paddleocr_vl_cloud_token":"c"}`,
			want: `{"mineru_api_key":"***","mineru_server_api_key":"***","paddleocr_vl_cloud_token":"***"}`,
		},
		{
			name: "cos secretId and secretKey",
			in:   `{"secretId":"cos-id","secretKey":"cos-key","region":"ap-guangzhou"}`,
			want: `{"secretId":"***","secretKey":"***","region":"ap-guangzhou"}`,
		},
		{
			name: "invite and registration urls",
			in:   `{"invite_url":"https://x/register?token=SECRET","registration_url":"https://x/register?token=SECRET"}`,
			want: `{"invite_url":"***","registration_url":"***"}`,
		},
		{
			name: "nested maps are redacted at any depth",
			in: `{"multimodal":{"vlm":{"apiKey":"sk-vlm"},"cos":{"secretId":"cos-id","secretKey":"cos-key",` +
				`"region":"ap-x"}}}`,
			want: `{"multimodal":{"vlm":{"apiKey":"***"},"cos":{"secretId":"***","secretKey":"***",` +
				`"region":"ap-x"}}}`,
		},
		{
			name: "objects inside arrays are redacted",
			in:   `{"models":[{"apiKey":"a"},{"name":"n","apiToken":"b"}]}`,
			want: `{"models":[{"apiKey":"***"},{"name":"n","apiToken":"***"}]}`,
		},
		{
			name: "credentials subtree is replaced whole",
			in:   `{"platform":"wecom","credentials":{"bot_id":"aib-1","bot_secret":"s3","bot_name":"小科"},"name":"x"}`,
			want: `{"platform":"wecom","credentials":"***","name":"x"}`,
		},
		{
			name: "headers subtree is replaced whole",
			in:   `{"headers":{"Authorization":"Bearer abc","X-Trace-Id":"t"}}`,
			want: `{"headers":"***"}`,
		},
		{
			name: "escaped quote inside a JSON secret value",
			in:   `{"password":"x\"y"}`,
			want: `{"password":"***"}`,
		},
		{
			name: "escaped unicode inside a JSON secret value",
			in:   "{\"password\":\"p\\u0022q\"}",
			want: `{"password":"***"}`,
		},
		{
			name: "wechat qrcode response credentials",
			in: `{"data":{"status":"confirmed","credentials":{"bot_token":"wx-secret",` +
				`"ilink_bot_id":"bot-1","ilink_user_id":"u-1"},"baseurl":"https://x"}}`,
			want: `{"data":{"status":"confirmed","credentials":"***","baseurl":"https://x"}}`,
		},
		{
			name: "non-JSON fallback keeps escaped value quoting intact",
			in:   `upstream error: {"password":"x\"y"} (status 400)`,
			want: `upstream error: {"password":"***"} (status 400)`,
		},
		{
			name: "non-JSON fallback redacts numeric and null values",
			in:   `body {"api_key":12345,"token":null}`,
			want: `body {"api_key":"***","token":"***"}`,
		},
		{
			name: "truncated JSON tail is still redacted",
			in:   `{"llm":{"apiKey":"sk-trunc`,
			want: `{"llm":{"apiKey":"***"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSanitized(t, tc.in, tc.want)
		})
	}
}

// The logging middleware is registered ahead of Auth and of every route
// handler, so whatever it buffers is buffered before a route's own body cap can
// refuse the request. These tests pin both halves of that: the middleware stays
// inside its own log budget, and the handler still reads every byte.

// countingBody records how many bytes a reader actually pulled, so a test can
// tell "the middleware peeked" from "the middleware slurped the request".
type countingBody struct {
	io.Reader
	read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { return nil }

// newJSONContext returns a context whose body is a countingBody, plus that
// counter, so a test can observe how much the middleware consumed.
func newJSONContext(t *testing.T, path, body string) (*gin.Context, *countingBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	counter := &countingBody{Reader: strings.NewReader(body)}
	c.Request.Body = counter
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, counter
}

func TestReadRequestBodyKeepsShortBodyIntact(t *testing.T) {
	payload := `{"query":"refund policy","apiKey":"sk-secret"}`

	c, _ := newJSONContext(t, "/api/v1/knowledge-search", payload)
	got := readRequestBody(c)

	want := `{"apiKey":"***","query":"refund policy"}`
	if got != want {
		t.Fatalf("readRequestBody() = %q, want %q", got, want)
	}
	rest, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if string(rest) != payload {
		t.Fatalf("handler saw %q, want the untouched body %q", rest, payload)
	}
}

// A request larger than the log budget must cost the middleware no more than the
// log budget (plus one byte, to tell "exactly at the limit" from "over it").
func TestReadRequestBodyStaysInsideItsLogBudget(t *testing.T) {
	payload := strings.Repeat("a", maxBodySize*8)

	c, counter := newJSONContext(t, "/api/v1/knowledge-search", payload)
	got := readRequestBody(c)

	if budget := maxBodySize + 1; counter.read > budget {
		t.Fatalf("middleware read %d bytes of a %d byte request, want at most %d",
			counter.read, len(payload), budget)
	}
	if want := maxBodySize + len(bodyTruncatedMarker); len(got) != want {
		t.Fatalf("logged body is %d bytes, want %d (%d logged + %q)",
			len(got), want, maxBodySize, bodyTruncatedMarker)
	}
	if !strings.HasSuffix(got, bodyTruncatedMarker) {
		t.Fatalf("logged body %q does not end with the truncation marker", got)
	}
	rest, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if len(rest) != len(payload) {
		t.Fatalf("handler saw %d bytes, want all %d of the request", len(rest), len(payload))
	}
}

// The peek must not become a cap the handler inherits: the route's own
// http.MaxBytesReader still sees the whole request and still decides.
func TestReadRequestBodyLeavesRouteBodyCapDeciding(t *testing.T) {
	const routeCap = 64 << 10
	payload := strings.Repeat("x", routeCap*2)

	c, _ := newJSONContext(t, "/api/v1/skills/catalog", payload)
	readRequestBody(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, routeCap)

	_, err := io.ReadAll(c.Request.Body)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("MaxBytesReader error = %v, want *http.MaxBytesError", err)
	}
}

func TestReadRequestBodyRedactsDataURI(t *testing.T) {
	payload := `{"image":"data:image/png;base64,` + strings.Repeat("A", maxBodySize) + `"}`

	c, _ := newJSONContext(t, "/api/v1/knowledge-search", payload)
	got := readRequestBody(c)

	if strings.Contains(got, strings.Repeat("A", 32)) {
		t.Fatalf("logged body still contains the base64 payload")
	}
	if !strings.Contains(got, "data:image/png;base64,[redacted ") {
		t.Fatalf("readRequestBody() = %q, want a redacted data URI", got)
	}
	rest, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if string(rest) != payload {
		t.Fatalf("handler saw a redacted body; the request itself must stay intact")
	}
}

func TestReadRequestBodySkipsNonTextBodies(t *testing.T) {
	payload := "--x\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nbody\r\n--x--\r\n"

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/file", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	if got := readRequestBody(c); got != "[非文本类型，已跳过]" {
		t.Fatalf("readRequestBody() = %q, want the non-text marker", got)
	}
	rest, err := io.ReadAll(c.Request.Body)
	if err != nil || string(rest) != payload {
		t.Fatalf("non-text body was disturbed: %q (%v)", rest, err)
	}
}

func TestSanitizeQuery(t *testing.T) {
	got := sanitizeQuery("code=secret-code&state=secret-state&next=%2Fsettings&state=second")
	want := "code=%2A%2A%2A&next=%2Fsettings&state=%2A%2A%2A"
	if got != want {
		t.Fatalf("sanitizeQuery() = %q, want %q", got, want)
	}
}

// The sandbox terminal presents its handshake credential as a query parameter
// because a browser WebSocket upgrade cannot carry Authorization. Holding that
// value for its TTL is enough to open a shell in the session's sandbox, so the
// redaction is a security boundary, not cosmetics.
func TestSanitizeQueryRedactsTerminalTicket(t *testing.T) {
	got := sanitizeQuery("ticket=eyJhbGciOiJIUzI1NiJ9.payload.signature&provision=1&cols=120")
	want := "cols=120&provision=1&ticket=%2A%2A%2A"
	if got != want {
		t.Fatalf("sanitizeQuery() = %q, want %q", got, want)
	}
	if strings.Contains(got, "signature") {
		t.Fatalf("sanitizeQuery() leaked the ticket: %q", got)
	}
}

func TestSkipsBodyLoggingForSecretRoutes(t *testing.T) {
	for _, p := range []string{
		"/api/v1/agent/secure-forms/p1",
		"/api/v1/me/credential-vault",
		"/api/v1/me/credential-vault/abc",
		"/api/v1/local-browser/x",
		"/api/v1/me/browser",
	} {
		if !skipsBodyLogging(p) {
			t.Errorf("%s must not have its body logged", p)
		}
	}
	for _, p := range []string{"/api/v1/agent/prompts/p1", "/api/v1/me/env-vars"} {
		if skipsBodyLogging(p) {
			t.Errorf("%s keeps its body log", p)
		}
	}
}

// /r/<token> is a short-lived capability URL. The token identifies the grant
// on its own, so the access log must not retain it.
func TestRedactResourcePathToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/r/abcDEF123_-", "/r/***"},
		{"/api/v1/r/abcDEF123_-", "/api/v1/r/***"},
		{"/r/", "/r/"},
		{"/api/v1/user/profile", "/api/v1/user/profile"},
		{"/api/v1/resources/list", "/api/v1/resources/list"},
	}
	for _, tc := range cases {
		if got := redactResourcePath(tc.in); got != tc.want {
			t.Errorf("redactResourcePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// End-to-end check that the access log line itself, not just the helpers,
// masks the /r/<token> capability and the presigned signature parameters.
func TestLoggerRedactsResourceTokenAndPresignedSignature(t *testing.T) {
	var buf bytes.Buffer
	logger.SetOutput(&buf)
	defer logger.SetOutput(os.Stdout)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Logger())
	r.GET("/r/:token", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet,
		"/r/capability-token-123?sig=presign-signature&expires=1700000000&file_path=local%3A%2F%2F1%2Fa.png", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, leaked := range []string{"capability-token-123", "presign-signature", "1700000000"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("access log leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "/r/***") {
		t.Fatalf("access log lost the resource path: %s", out)
	}
}

// A request far over the preview cap is truncated before sanitisation, which
// leaves invalid JSON. The structured redactor cannot run then, and the scalar
// regex does not know the headers/credentials containers -- so a credential
// header in the preview must still be masked by the fallback path.
func TestTruncatedPreviewStillMasksCredentialContainers(t *testing.T) {
	body := `{"service":{"headers":{"X-Session":"synthetic_secret"},"name":"` +
		strings.Repeat("x", 400) + `","headers":{"Authorization":"Bearer synthetic_bearer"}}`
	got := sanitizeBody(body)
	if strings.Contains(got, "synthetic_secret") || strings.Contains(got, "synthetic_bearer") {
		t.Fatalf("credential leaked through the truncated preview: %s", got)
	}
}
