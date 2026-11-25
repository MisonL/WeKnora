package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	maxBodySize = 1024 * 10 // 最大记录10KB的body内容
	// bodyTruncatedMarker 是日志里跟在被截断的 body 后面的标记。
	bodyTruncatedMarker = "... [内容过长，已截断]"
)

// replayedBody 把日志窥探消费掉的字节还给 handler，让它读到的 body 与没有这层
// 中间件时完全一致；Close 仍然关闭原始 body，连接得以释放。
type replayedBody struct {
	io.Reader
	io.Closer
}

// loggerResponseBodyWriter 自定义ResponseWriter用于捕获响应内容（用于logger中间件）
type loggerResponseBodyWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

// Write 重写Write方法，同时写入buffer和原始writer
// 限制buffer大小，避免SSE等流式响应导致内存无限增长
func (r loggerResponseBodyWriter) Write(b []byte) (int, error) {
	if r.body.Len() < maxBodySize {
		remaining := maxBodySize - r.body.Len()
		if len(b) <= remaining {
			r.body.Write(b)
		} else {
			r.body.Write(b[:remaining])
		}
	}
	return r.ResponseWriter.Write(b)
}

// sensitiveJSONFieldNames lists keys whose value is replaced with "***".
// Comparison is on a normalized key (lowercased, "_" and "-" removed), so one
// entry covers snake_case, kebab-case, camelCase and PascalCase spellings.
//
// IM channel credentials do not follow the api_key/secret naming (see the
// creds keys read by internal/im/*/factory.go): 企业微信 bot_secret、飞书
// app_secret、钉钉 app_secret/signing_secret、Telegram bot_token、Slack
// signing_secret 等，必须逐个列出，否则创建渠道时的请求体会把明文密钥写进访问日志。
var sensitiveJSONFieldNames = []string{
	"new_password", "old_password", "password", "passwd", "ticket", "token",
	"access_token", "refresh_token", "next_token", "device_token",
	"pending_token", "pairing_link", "id_token", "authorization",
	"auth_token", "api_key", "api_secret", "secret_key", "client_secret",
	"private_key", "secret", "authorization_url", "authorization_attempt",
	"assertion", "bot_secret", "bot_token", "app_secret", "app_token",
	"agent_secret", "encoding_aes_key", "encrypt_key", "verification_token",
	"signing_secret", "outgoing_token", "secret_token", "webhook_secret",
	// Initialization payloads (model providers + COS/MinIO) and invitation
	// links. The values are plaintext credentials or bearer links.
	"app_password", "access_key_id", "access_key", "secret_access_key",
	"secret_id", "api_token", "mineru_api_key", "mineru_server_api_key",
	"paddleocr_vl_cloud_token", "invite_url", "registration_url",
}

// sensitiveJSONWholeValueKeys replace the entire subtree, not just a scalar:
// these objects exist to carry credentials and every leaf matters.
var sensitiveJSONWholeValueKeys = map[string]struct{}{
	"credentials": {},
	"headers":     {},
}

// sensitiveJSONFields is the normalized key set used by the JSON-aware
// redactor.
var sensitiveJSONFields = func() map[string]struct{} {
	out := make(map[string]struct{}, len(sensitiveJSONFieldNames))
	for _, name := range sensitiveJSONFieldNames {
		out[normalizeSensitiveKey(name)] = struct{}{}
	}
	return out
}()

// normalizeSensitiveKey lowercases a JSON key and drops "_"/"-" so camelCase,
// snake_case and kebab-case spellings of the same field compare equal.
func normalizeSensitiveKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sensitiveFieldRegex is the fallback for bodies that are not valid JSON
// (truncated previews, plain-text upstream errors, form-encoded bodies).
// $1 captures the original field name (including quotes) so the log keeps the
// field name and only loses the value. The value pattern accepts escaped
// quotes (\"), numbers, null/booleans, and an unterminated string running to
// the end of a truncated body.
var sensitiveFieldRegex = buildSensitiveFieldRegex()

func buildSensitiveFieldRegex() *regexp.Regexp {
	names := make([]string, 0, len(sensitiveJSONFieldNames))
	for _, name := range sensitiveJSONFieldNames {
		// "_" becomes an optional separator so apikey/apiKey/api_key/api-key
		// all match under the case-insensitive flag.
		names = append(names, strings.ReplaceAll(name, "_", "[_-]?"))
	}
	pattern := `(?i)("(?:` + strings.Join(names, "|") + `)")\s*:\s*(?:` +
		`"(?:[^"\\]|\\.)*"` + // quoted string (escapes handled)
		`|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?` + // number
		`|null|true|false` + // literals
		`|"(?:[^"\\]|\\.)*\z` + // unterminated string at the truncation edge
		`)`
	return regexp.MustCompile(pattern)
}

// dataURIRegex matches an inline data URI whose payload is base64. Request logs
// are capped at 10KB, and an image upload still fills that cap with file bytes.
var dataURIRegex = regexp.MustCompile(`(?i)\bdata:[^\s"'<>]{0,128};base64,[A-Za-z0-9+/=\r\n]*`)

// resourceTokenPathRegex matches the /r/<token> capability URL used for
// short-lived resource grants. The token alone authorizes the download, so it
// must not survive in the access log. The leading group keeps the separator
// (and does not match an "r/" inside a longer segment such as "user/").
var resourceTokenPathRegex = regexp.MustCompile(`(^|/)r/[A-Za-z0-9_-]+`)

// redactResourcePath masks a /r/<token> path segment.
func redactResourcePath(path string) string {
	return resourceTokenPathRegex.ReplaceAllString(path, "${1}r/***")
}

// envVarValueSensitiveKeys are the extra keys that carry a personal secret
// only on the /me/env-vars surface. Elsewhere "value" is an ordinary field.
var envVarValueSensitiveKeys = map[string]struct{}{"value": {}}

// envVarValueFieldRegex is the fallback redactor for path-specific keys; it
// mirrors sensitiveFieldRegex's quote-aware value shapes.
var envVarValueFieldRegex = buildExtraFieldRegex(envVarValueSensitiveKeys)

// extraSensitiveKeysForPath returns path-specific sensitive keys on top of
// the shared set.
func extraSensitiveKeysForPath(path string) map[string]struct{} {
	if path == "/api/v1/me/env-vars" || strings.HasPrefix(path, "/api/v1/me/env-vars/") {
		return envVarValueSensitiveKeys
	}
	return nil
}

// sanitizeBody 清理敏感信息，并把 data URI 的 base64 载荷换成长度标记。
//
// JSON documents take the structured path: keys are matched after
// normalization, nested maps and arrays are traversed, and credential
// containers (credentials/headers) are replaced whole. Anything else —
// truncated previews, plain-text errors, form bodies — falls back to the
// token regex, which is quote-aware.
func sanitizeBody(body string) string {
	return sanitizeBodyForPath(body, "")
}

// sanitizeBodyForPath is sanitizeBody with path-aware extra keys (see
// extraSensitiveKeysForPath).
func sanitizeBodyForPath(body, path string) string {
	redacted := dataURIRegex.ReplaceAllStringFunc(body, redactDataURI)
	extra := extraSensitiveKeysForPath(path)
	if out, ok := sanitizeJSONBody(redacted, extra); ok {
		return out
	}
	out := sensitiveFieldRegex.ReplaceAllString(redacted, `$1:"***"`)
	return redactExtraJSONFields(out, extra)
}

// redactExtraJSONFields applies the path-specific keys to a body that failed
// JSON parsing (typically a truncated preview), using the same quote-aware
// value shapes as the shared regex.
func redactExtraJSONFields(body string, extra map[string]struct{}) string {
	if len(extra) == 0 {
		return body
	}
	return envVarValueFieldRegex.ReplaceAllString(body, `$1:"***"`)
}

// buildExtraFieldRegex compiles the fallback redactor for a path-specific key
// set.
func buildExtraFieldRegex(extra map[string]struct{}) *regexp.Regexp {
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, regexp.QuoteMeta(name))
	}
	pattern := `(?i)("(?:` + strings.Join(names, "|") + `)")\s*:\s*(?:` +
		`"(?:[^"\\]|\\.)*"` +
		`|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?` +
		`|null|true|false` +
		`|"(?:[^"\\]|\\.)*\z` +
		`)`
	return regexp.MustCompile(pattern)
}

// sanitizeFormBody redacts an application/x-www-form-urlencoded body. Keys are
// compared against the same normalized sensitive set as JSON bodies, so
// token/api_key/corpsecret-style fields are masked while ordinary fields keep
// their value.
func sanitizeFormBody(body string, extra map[string]struct{}) string {
	body = dataURIRegex.ReplaceAllStringFunc(body, redactDataURI)
	suffix := ""
	if strings.HasSuffix(body, bodyTruncatedMarker) {
		body = strings.TrimSuffix(body, bodyTruncatedMarker)
		suffix = bodyTruncatedMarker
	}

	values, err := url.ParseQuery(body)
	if err != nil {
		return sanitizeFormBodyFallback(body, extra) + suffix
	}

	masked := false
	for key := range values {
		if isSensitiveLogKey(key, extra) {
			values[key] = []string{"***"}
			masked = true
		}
	}
	if !masked {
		return body + suffix
	}
	return values.Encode() + suffix
}

// sanitizeFormBodyFallback handles a form body that url.ParseQuery rejected
// (typically a truncated body ending inside a percent escape).
func sanitizeFormBodyFallback(body string, extra map[string]struct{}) string {
	parts := strings.Split(body, "&")
	for i, part := range parts {
		key, _, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		decoded := key
		if unescaped, err := url.QueryUnescape(key); err == nil {
			decoded = unescaped
		}
		if isSensitiveLogKey(decoded, extra) {
			parts[i] = key + "=***"
		}
	}
	return strings.Join(parts, "&")
}

// isSensitiveLogKey reports whether a (decoded) key name belongs to the shared
// sensitive set or the path-specific extra set.
func isSensitiveLogKey(key string, extra map[string]struct{}) bool {
	normalized := normalizeSensitiveKey(key)
	if _, sensitive := sensitiveJSONFields[normalized]; sensitive {
		return true
	}
	_, sensitive := extra[normalized]
	return sensitive
}

// sanitizeJSONBody redacts a valid JSON document and re-encodes it. Numbers
// are decoded as json.Number so large ids keep their exact spelling. The
// second Decode guards against a valid prefix followed by garbage, which must
// not be silently dropped from the log.
func sanitizeJSONBody(body string, extra map[string]struct{}) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", false
	}
	encoded, err := json.Marshal(redactJSONValue(value, extra))
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func redactJSONValue(value any, extra map[string]struct{}) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := normalizeSensitiveKey(key)
			if _, whole := sensitiveJSONWholeValueKeys[normalized]; whole {
				typed[key] = "***"
				continue
			}
			if isSensitiveLogKey(key, extra) {
				typed[key] = "***"
				continue
			}
			typed[key] = redactJSONValue(child, extra)
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = redactJSONValue(typed[i], extra)
		}
		return typed
	default:
		return value
	}
}

func redactDataURI(match string) string {
	marker := ";base64,"
	idx := strings.Index(strings.ToLower(match), marker)
	if idx < 0 {
		return "data:[redacted]"
	}
	payloadLen := len(match) - idx - len(marker)
	return fmt.Sprintf("%s;base64,[redacted %d chars]", match[:idx], payloadLen)
}

// sanitizeQuery prevents OAuth authorization codes, CSRF/attempt state, and
// handshake credentials from being copied into access logs. The shared
// implementation also backs the Langfuse trace metadata so both surfaces
// redact the same parameter set.
func sanitizeQuery(raw string) string {
	return secutils.SanitizeQuery(raw)
}

// readRequestBody 读取请求体（限制大小用于日志，但把剩余字节原样交还后续handler）
func readRequestBody(c *gin.Context) string {
	if c.Request.Body == nil {
		return ""
	}

	// 检查Content-Type，只记录JSON类型
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/json") &&
		!strings.Contains(contentType, "application/x-www-form-urlencoded") &&
		!strings.Contains(contentType, "text/") {
		return "[非文本类型，已跳过]"
	}

	// 重置request body：把已窥探的前缀接回未读取的剩余部分，handler 仍能读到完整
	// 数据，但中间件自身只驻留日志需要的那点字节。
	//
	// 这里原本是无上限的 io.ReadAll(c.Request.Body)。Logger 注册在 Auth 之前
	// （router.go NewRouter），因此对所有请求执行，而任何路由的
	// http.MaxBytesReader（limitJSONBody / limitUploadBody / browserskill 的 4 KiB
	// 授权接口）都是在 handler 里才装上的：body 早已被整体读进内存，那些 cap 只剩
	// 报错的意义，没有限住内存的作用。没有 cap 的路由（对话、检索、IM webhook、
	// MCP 端点）则连上限都不存在。
	bodyBytes, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodySize+1))
	originalBody := c.Request.Body
	c.Request.Body = &replayedBody{
		Reader: io.MultiReader(bytes.NewReader(bodyBytes), originalBody),
		Closer: originalBody,
	}
	if err != nil {
		return "[读取请求体失败]"
	}

	// 用于日志的body（限制大小）
	truncated := len(bodyBytes) > maxBodySize
	if truncated {
		bodyBytes = bodyBytes[:maxBodySize]
	}

	bodyStr := string(bodyBytes)
	if truncated {
		bodyStr += bodyTruncatedMarker
	}

	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		return sanitizeFormBody(bodyStr, extraSensitiveKeysForPath(c.Request.URL.Path))
	}
	return sanitizeBodyForPath(bodyStr, c.Request.URL.Path)
}

// RequestID middleware adds a unique request ID to the context
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get request ID from header or generate a new one
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		safeRequestID := secutils.SanitizeForLog(requestID)
		// Set request ID in header
		c.Header("X-Request-ID", requestID)

		// Set request ID in context
		c.Set(types.RequestIDContextKey.String(), requestID)

		// Set logger in context
		requestLogger := logger.GetLogger(c)
		requestLogger = requestLogger.WithField("request_id", safeRequestID)
		c.Set(types.LoggerContextKey.String(), requestLogger)

		// Set request ID in the global context for logging
		c.Request = c.Request.WithContext(
			context.WithValue(
				context.WithValue(c.Request.Context(), types.RequestIDContextKey, requestID),
				types.LoggerContextKey, requestLogger,
			),
		)

		c.Next()
	}
}

// skipsBodyLogging reports routes whose bodies carry credentials, page
// content or screenshots. Their access metadata is still logged; their
// bodies are never read or buffered. sanitizeBody is no substitute here: it
// only knows fixed key names, so a credential under an unknown field name
// (or free-form page text) would still be logged.
func skipsBodyLogging(path string) bool {
	return strings.HasPrefix(path, "/api/v1/local-browser/") ||
		path == "/api/v1/me/browser" || strings.HasSuffix(path, "/local-browser") ||
		strings.HasPrefix(path, "/api/v1/agent/secure-forms/") ||
		path == "/api/v1/me/credential-vault" || strings.HasPrefix(path, "/api/v1/me/credential-vault/")
}

// Logger middleware logs request details with request ID, input and output
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		isWikiStats := strings.HasPrefix(path, "/api/v1/knowledgebase/") && strings.HasSuffix(path, "/wiki/stats")
		if strings.HasPrefix(path, "/assets/") || isWikiStats {
			c.Next()
			return
		}

		browserTraffic := skipsBodyLogging(path)
		// 读取请求体（在Next之前读取，因为Next会消费body）
		var requestBody string
		if !browserTraffic && (c.Request.Method == "POST" || c.Request.Method == "PUT" || c.Request.Method == "PATCH") {
			requestBody = readRequestBody(c)
		}

		// 创建响应体捕获器
		responseBody := &bytes.Buffer{}
		responseWriter := &loggerResponseBodyWriter{
			ResponseWriter: c.Writer,
			body:           responseBody,
		}
		if !browserTraffic {
			c.Writer = responseWriter
		}

		// Process request
		c.Next()

		// Get request ID from context
		requestID, exists := c.Get(types.RequestIDContextKey.String())
		requestIDStr := "unknown"
		if exists {
			if idStr, ok := requestID.(string); ok && idStr != "" {
				requestIDStr = idStr
			}
		}
		safeRequestID := secutils.SanitizeForLog(requestIDStr)

		// Calculate latency
		latency := time.Since(start)

		// Get client IP and status code
		clientIP := c.ClientIP()
		statusCode := c.Writer.Status()
		method := c.Request.Method

		if raw != "" {
			path = redactResourcePath(path) + "?" + sanitizeQuery(raw)
		} else {
			path = redactResourcePath(path)
		}

		// 读取响应体
		responseBodyStr := ""
		if responseBody.Len() > 0 {
			contentType := c.Writer.Header().Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				responseBodyStr = "[SSE流式响应，已跳过]"
			} else if strings.Contains(contentType, "application/json") ||
				strings.Contains(contentType, "text/") {
				bodyBytes := responseBody.Bytes()
				if len(bodyBytes) >= maxBodySize {
					responseBodyStr = string(bodyBytes[:maxBodySize]) + "... [内容过长，已截断]"
				} else {
					responseBodyStr = string(bodyBytes)
				}
				responseBodyStr = sanitizeBody(responseBodyStr)
			} else {
				responseBodyStr = "[非文本类型，已跳过]"
			}
		}

		// 构建日志消息
		logMsg := logger.GetLogger(c)
		logMsg = logMsg.WithFields(map[string]interface{}{
			"request_id":  safeRequestID,
			"method":      method,
			"path":        secutils.SanitizeForLog(path),
			"status_code": statusCode,
			"size":        c.Writer.Size(),
			"latency":     latency.String(),
			"client_ip":   secutils.SanitizeForLog(clientIP),
		})

		// 添加请求体（如果有）
		if requestBody != "" {
			logMsg = logMsg.WithField("request_body", secutils.SanitizeForLog(requestBody))
		}

		// 添加响应体（如果有）
		if responseBodyStr != "" {
			logMsg = logMsg.WithField("response_body", secutils.SanitizeForLog(responseBodyStr))
		}
		if last := c.Errors.Last(); last != nil && last.Err != nil {
			logMsg = logMsg.WithField("error", secutils.SanitizeForLog(last.Err.Error()))
		}
		switch {
		case statusCode >= 500:
			logMsg.Error()
		case statusCode >= 400:
			logMsg.Warn()
		default:
			logMsg.Info()
		}
	}
}
