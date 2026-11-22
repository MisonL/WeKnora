// Package client provides the implementation for interacting with the WeKnora API
// This package encapsulates CRUD operations for server resources and provides a friendly interface for callers
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the client for interacting with the WeKnora service.
//
// Authentication uses one of two credential kinds:
//   - API key (long-lived, set via X-API-Key)        — see WithAPIKey
//   - Bearer JWT (short-lived, set via Authorization)— see WithBearerToken
//
// Both may be configured simultaneously; X-API-Key takes precedence at the
// HTTP layer. The legacy WithToken is kept as an alias for WithAPIKey for two
// minor versions of compatibility.
type Client struct {
	baseURL    string
	httpClient *http.Client
	// streamTimeout is zero by default so long-lived streams and downloads are not
	// severed by the ordinary request timeout. An explicit WithTimeout option
	// sets it, preserving that option's all-request semantics.
	streamTimeout time.Duration
	// uploadTimeout is the value uploadPinned applies; it is meaningless under
	// the other policies. Uploads get no ceiling of their own by default: the
	// 30-second http.Client default is sized for JSON request/response calls
	// and severs any upload whose transfer does not finish in half a minute, so
	// the default policy derives one from the payload size instead (see
	// uploadTimeoutFor).
	uploadTimeout time.Duration
	// uploadPolicy is the single source of truth for how the upload ceiling is
	// chosen. Collapsing the choices into one field is what makes the options
	// order-independent: each option overwrites the whole policy instead of
	// poking at flags a later option may or may not have set.
	uploadPolicy uploadPolicy
	// uploadSource records which option set uploadPolicy, so a repeated
	// WithTimeout replaces its own earlier decision without ever overriding a
	// WithUploadTimeout.
	uploadSource uploadSource
	// generalTimeoutSet records an explicit WithTimeout, which lets a negative
	// WithUploadTimeout tell "the caller pinned 0" from "the 30-second default
	// is still in place".
	generalTimeoutSet bool
	apiKey            string
	bearerToken       string
	tenantID          *uint64
}

// ClientOption defines client configuration options
type ClientOption func(*Client)

// uploadPolicy says where the upload ceiling comes from.
type uploadPolicy int

const (
	// uploadDerived is the default: a ceiling computed from the payload size.
	uploadDerived uploadPolicy = iota
	// uploadPinned uses uploadTimeout verbatim, zero included. A zero means
	// "no upload timeout", exactly as it does on http.Client.
	uploadPinned
	// uploadNone turns the ceiling off explicitly.
	uploadNone
)

// uploadSource says which option decided the current policy. It exists because
// uploadPinned/uploadNone can come from either option, and a later WithTimeout
// must be able to tell "a dedicated option owns this" (leave it alone) from
// "a previous WithTimeout set it, so replace it".
type uploadSource int

const (
	sourceNone      uploadSource = iota // no option touched uploads
	sourceGeneral                       // the value came from WithTimeout
	sourceDedicated                     // the value came from WithUploadTimeout
)

// WithUploadTimeout pins the ceiling for multipart uploads instead of deriving
// it from the payload size. Use it when the transfer rate is known to be worse
// than the pessimistic floor uploadTimeoutFor assumes, or when the derived
// ceiling is too generous for a caller that needs a hard bound.
//
// It takes precedence over WithTimeout for uploads regardless of the order the
// options are applied in, and a zero value means "no upload timeout" exactly as
// it does on http.Client. Pass a negative value to clear the explicit setting,
// which hands uploads back to WithTimeout the same way leaving this option out
// would -- whichever order the two are listed in.
func WithUploadTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout < 0 {
			// Not a ceiling of its own: forget the dedicated setting and let
			// the ordinary timeout decide, exactly as if this option had never
			// been passed. That means undoing a WithTimeout that had already
			// been folded in, not just clearing a dedicated one -- otherwise
			// T(0), U(-1) and U(-1), T(0) would disagree.
			c.uploadTimeout = 0
			switch {
			case !c.generalTimeoutSet:
				// Nothing pins the ordinary timeout either: uploads derive, as
				// they would with no options at all. The 30-second default must
				// not be mistaken for a caller's choice.
				c.uploadPolicy = uploadDerived
				c.uploadSource = sourceNone
			case c.httpClient.Timeout > 0:
				c.uploadPolicy = uploadPinned
				c.uploadSource = sourceGeneral
				c.uploadTimeout = c.httpClient.Timeout
			default:
				c.uploadPolicy = uploadNone
				c.uploadSource = sourceGeneral
			}
			return
		}
		c.uploadPolicy = uploadPinned
		c.uploadSource = sourceDedicated
		c.uploadTimeout = timeout
	}
}

// WithTimeout sets the timeout for ordinary, streaming and upload HTTP
// requests.
//
// Streaming requests have no timeout by default; callers that set this option
// explicitly are asking for the same upper bound to apply to streams and
// downloads. Uploads do not inherit the ordinary 30-second default — they
// derive a ceiling from the payload size instead — but an explicit value here
// does bound them, so a caller who pins a deadline is not silently exceeded.
// WithUploadTimeout overrides this for uploads alone, and does so whether it is
// applied before or after this option.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = timeout
		c.streamTimeout = timeout
		c.generalTimeoutSet = true
		if c.uploadSource == sourceDedicated {
			// A dedicated upload option already decided this; honour it
			// whatever order the caller listed the options in.
			return
		}
		// Uploads follow the ordinary timeout. A zero here means the caller
		// asked for no bound at all, which is what uploads get -- not the
		// derived ceiling, which they only inherit from the 30-second default
		// this option just replaced.
		if timeout == 0 {
			c.uploadPolicy = uploadNone
			c.uploadSource = sourceGeneral
			c.uploadTimeout = 0
			return
		}
		c.uploadPolicy = uploadPinned
		c.uploadSource = sourceGeneral
		c.uploadTimeout = timeout
	}
}

// WithTransport overrides the underlying http.RoundTripper on the SDK's
// HTTP client. The default Timeout (and any WithTimeout override) is
// preserved. Intended for callers (the CLI's authretry layer; metrics or
// signing middleware) that want to wrap the transport without replacing
// the whole http.Client.
//
// Passing nil restores http.DefaultTransport.
func WithTransport(rt http.RoundTripper) ClientOption {
	return func(c *Client) {
		c.httpClient.Transport = rt
	}
}

// WithAPIKey sets the long-lived API key sent as the X-API-Key header.
func WithAPIKey(key string) ClientOption {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithBearerToken sets the JWT bearer token sent as the Authorization header.
// Used after a successful auth.login to call authenticated endpoints with the
// short-lived access token.
func WithBearerToken(token string) ClientOption {
	return func(c *Client) {
		c.bearerToken = token
	}
}

// WithToken is the v0.x compatibility alias for WithAPIKey. Prefer WithAPIKey
// (or WithBearerToken for JWT). Will be removed in the next major; the alias
// is preserved for two minor versions per ADR.
//
// Deprecated: use WithAPIKey for X-API-Key, WithBearerToken for JWT.
func WithToken(token string) ClientOption {
	return WithAPIKey(token)
}

// WithTenantID sets X-Tenant-ID on every request. Use only for explicit
// cross-tenant access by callers with CanAccessAllTenants — the server's
// auth middleware runs the cross-tenant gate whenever this header is
// present on a bearer request, even when the value matches the credential's
// own tenant, and 403s normal users. JWT bearer tokens and tenant-scoped
// API keys carry tenant identity intrinsically, so the header is redundant
// (and harmful) for default-tenant traffic. Per-request override via the
// "TenantID" context value still applies.
func WithTenantID(tenantID uint64) ClientOption {
	return func(c *Client) {
		c.tenantID = &tenantID
	}
}

// NewClient creates a new client instance
func NewClient(baseURL string, options ...ClientOption) *Client {
	client := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	for _, option := range options {
		option(client)
	}

	return client
}

// buildRequest constructs the authenticated *http.Request shared by doRequest
// and doRequestStream: serializes the JSON body, composes URL + query, and
// applies auth headers.
func (c *Client) buildRequest(ctx context.Context,
	method, path string, body interface{}, query url.Values,
) (*http.Request, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	u := fmt.Sprintf("%s%s", c.baseURL, path)
	if len(query) > 0 {
		u = fmt.Sprintf("%s?%s", u, query.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	c.applyAuthHeaders(ctx, req)
	return req, nil
}

// doRequest executes an HTTP request subject to the client's blanket Timeout
// (default 30s). Use for ordinary request/response calls.
func (c *Client) doRequest(ctx context.Context,
	method, path string, body interface{}, query url.Values,
) (*http.Response, error) {
	req, err := c.buildRequest(ctx, method, path, body, query)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// Upload ceilings.
//
// The ordinary 30-second timeout describes a JSON request/response round trip.
// A multipart body is transferred at whatever rate the link offers, so the same
// ceiling turns a slow-but-healthy upload into a client-side abort: at ~5 Mbps
// a 50 MiB file needs over 80 seconds. The server rejects a multipart body it
// did not fully receive (FormFile fails and the handler returns before any
// knowledge row or task exists), so the cost is a failed upload rather than a
// stuck job — but it is still a failure the client could have avoided.
// Uploads therefore get their own ceiling, derived from the payload size.
//
// The derived ceiling assumes the upload has the link to itself. N uploads
// running concurrently share the link, so each gets 1/N of it; a caller that
// uploads in parallel should set WithUploadTimeout from its measured rate
// rather than rely on the derivation.
const (
	// uploadBaseTimeout covers connection setup, the server's own synchronous
	// work (saving the file, writing the row, enqueuing the task), and reading
	// the small JSON response.
	uploadBaseTimeout = 60 * time.Second
	// uploadMinThroughput is the slowest per-upload transfer rate the derived
	// ceiling assumes, in bytes per second. Deliberately pessimistic: a link
	// that is slower than this for a whole upload is worth failing, and the
	// ceiling should not be the thing that decides.
	uploadMinThroughput = 100 * 1024
	// uploadMaxDerived is the largest derived ceiling. Past this the backstop
	// stops detecting a stalled transfer anyway, and an overflowing Duration
	// would turn into a negative Timeout, which net/http reads as "no timeout".
	uploadMaxDerived = 2 * time.Hour
)

// uploadTimeoutFor returns the ceiling for a multipart upload carrying size
// bytes. It grows with the payload so that raising the size limit does not
// silently re-introduce the abort this policy exists to prevent.
func uploadTimeoutFor(size int64) time.Duration {
	if size <= 0 {
		return uploadBaseTimeout
	}
	// Divide before multiplying: size is int64 bytes, and going through
	// seconds first keeps the product far from Duration's range.
	seconds := size / uploadMinThroughput
	if seconds >= int64((uploadMaxDerived-uploadBaseTimeout)/time.Second) {
		// Clamp the total, not just the transfer allowance: the base timeout
		// is part of the ceiling, so a payload that saturates the transfer
		// budget must still come out at or below uploadMaxDerived.
		return uploadMaxDerived
	}
	return uploadBaseTimeout + time.Duration(seconds)*time.Second
}

// doUpload sends a multipart request under the upload ceiling. The Transport is
// shared with c.httpClient so connection pooling is unaffected — in practice
// that is http.DefaultTransport, since NewClient leaves Transport nil.
//
// A zero ceiling means no timeout — the same meaning it has on http.Client. It
// is reached by an explicit WithUploadTimeout(0), a WithTimeout(0), or a
// WithUploadTimeout(negative) that leaves an explicit WithTimeout(0) in charge;
// uploadNone is what marks it, so the derived branch never produces zero.
func (c *Client) doUpload(req *http.Request, bodySize int64) (*http.Response, error) {
	uc := *c.httpClient // shallow copy: shares Transport, override Timeout
	switch c.uploadPolicy {
	case uploadNone:
		uc.Timeout = 0
	case uploadPinned:
		uc.Timeout = c.uploadTimeout
	default:
		// No option touched the upload ceiling, so the client still carries the
		// 30-second default that JSON calls want and uploads must not have.
		uc.Timeout = uploadTimeoutFor(bodySize)
	}
	return uc.Do(req)
}

// doRequestStream executes a streaming (SSE) request without the client's
// default 30-second blanket Timeout. http.Client.Timeout covers reading the
// response body, so applying that default would sever long-running chat /
// session-ask / continue-stream responses. Stream lifetime is governed by ctx
// unless the caller explicitly supplied WithTimeout, in which case that upper
// bound is preserved. The Transport is shared with c.httpClient so connection
// pooling is unaffected.
func (c *Client) doRequestStream(ctx context.Context,
	method, path string, body interface{}, query url.Values,
) (*http.Response, error) {
	req, err := c.buildRequest(ctx, method, path, body, query)
	if err != nil {
		return nil, err
	}
	sc := *c.httpClient // shallow copy: shares Transport, override Timeout
	sc.Timeout = c.streamTimeout
	return sc.Do(req)
}

// applyAuthHeaders sets X-API-Key, Authorization, X-Request-ID, and X-Tenant-ID
// on req based on client config and ctx values. Used by doRequest and any
// caller that builds its own *http.Request (currently CreateKnowledgeFromFile,
// which uses multipart and can't go through doRequest).
func (c *Client) applyAuthHeaders(ctx context.Context, req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	if requestID := ctx.Value("RequestID"); requestID != nil {
		if s, ok := requestID.(string); ok {
			req.Header.Set("X-Request-ID", s)
		}
	}

	tenantID := c.tenantID
	if ctxTenant := ctx.Value("TenantID"); ctxTenant != nil {
		switch v := ctxTenant.(type) {
		case *uint64:
			if v != nil {
				tenantID = v
			}
		case uint64:
			tenantID = &v
		case string:
			if parsed, err := strconv.ParseUint(v, 10, 64); err == nil {
				tenantID = &parsed
			}
		}
	}
	if tenantID != nil {
		req.Header.Set("X-Tenant-ID", strconv.FormatUint(*tenantID, 10))
	}
}

// Raw performs a raw HTTP request against the WeKnora API with the client's
// auth headers, X-Request-ID, and X-Tenant-ID injection applied.
//
// Experimental: this method is intended for one-off integrations and the
// `weknora api` CLI passthrough. The signature, return type, and behavior may
// change in any minor version. Prefer typed methods (ListKnowledgeBases,
// GetKnowledgeBase, etc.) when they exist.
func (c *Client) Raw(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	return c.doRequest(ctx, method, path, body, nil)
}

// APIError is a typed non-2xx HTTP response from the WeKnora API. Recover it
// with errors.As to branch on the HTTP status or the server's structured error
// code without parsing the Error() string:
//
//	var apiErr *client.APIError
//	if errors.As(err, &apiErr) && apiErr.StatusCode == 404 { ... }
//
// Error() intentionally preserves the legacy "HTTP error <status>: <body>"
// format so existing string-matching consumers keep working unchanged.
type APIError struct {
	StatusCode int    // HTTP status (401, 404, 409, 429, 500, …)
	Body       string // raw response body
	Code       int    // server's structured error code ({"code":N}); 0 when absent/non-JSON
	// RetryAfter is the response's Retry-After header in seconds, or 0 when the
	// server sent none or a form this client does not parse. A 429 carries it
	// so a caller can wait the stated time instead of backing off blindly.
	RetryAfter int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP error %d: %s", e.StatusCode, e.Body)
}

// Server error codes are the values of the {"code":N} field in an error
// response body, surfaced as APIError.Code. These mirror the generic codes in
// the server's internal/errors/errors.go so consumers branch on a named
// constant instead of a magic number. Domain-specific codes (2xxx+) are
// omitted until a consumer needs to branch on one.
const (
	ServerErrBadRequest         = 1000
	ServerErrUnauthorized       = 1001
	ServerErrForbidden          = 1002
	ServerErrNotFound           = 1003
	ServerErrMethodNotAllowed   = 1004
	ServerErrConflict           = 1005
	ServerErrTooManyRequests    = 1006
	ServerErrInternalServer     = 1007
	ServerErrServiceUnavailable = 1008
	ServerErrTimeout            = 1009
	ServerErrValidation         = 1010
	// ServerErrKnowledgeEnqueueFailed (2500) is the response to an upload that
	// was stored but whose parse job could not be queued. The row exists, so a
	// caller should reparse it rather than upload the file again.
	ServerErrKnowledgeEnqueueFailed = 2500
)

// newAPIError builds an *APIError from a non-2xx response's status and raw body,
// extracting the server's structured {"code":N} when the body is JSON.
func newAPIError(status int, body []byte) *APIError {
	return &APIError{StatusCode: status, Body: string(body), Code: extractServerCode(body)}
}

// newAPIErrorFromResponse is newAPIError plus the Retry-After hint. Every call
// site that still has the *http.Response uses this one, so a 429's header
// reaches the caller as APIError.RetryAfter.
func newAPIErrorFromResponse(resp *http.Response, body []byte) *APIError {
	if resp == nil {
		return newAPIError(0, body)
	}
	err := newAPIError(resp.StatusCode, body)
	err.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return err
}

// parseRetryAfter reads the delta-seconds form of Retry-After. The HTTP-date
// form is not parsed: this SDK's callers are short-lived requests, and a
// missing hint degrades to their own backoff rather than a wrong wait.
func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// maxErrorBodyBytes caps how much of a failed response body the SDK reads.
//
// A non-2xx body is copied verbatim into APIError.Body and interpolated into
// APIError.Error(), which callers log or print. A misconfigured server, a
// hostile origin, or a proxy serving an HTML error page can make that body
// arbitrarily large, so the SDK would both hold it in memory and hand the caller
// an unbounded error string. 1 MiB matches the cap internal/models/api applies
// to its own error bodies and sits far above any structured error the server
// emits.
const maxErrorBodyBytes = 1 << 20

// readErrorBody reads the diagnostic body of a failed response, bounded by
// maxErrorBodyBytes.
//
// Truncating here costs nothing: callers branch on the status code and on
// APIError.Code, and the body exists only to explain the failure. The read error
// is dropped, as before, so a broken connection cannot mask the HTTP status.
func readErrorBody(r io.Reader) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	return body
}

// maxResponseBodyBytes caps how much of a successful response body the SDK
// decodes.
//
// A non-error status still lets the server — or a proxy in front of it — decide
// how many bytes the SDK buffers and unmarshals: every helper decodes through
// parseResponse, so an oversized envelope would be held in memory before
// json.Decoder saw the end of it. 16 MiB matches the cap the Feishu and
// DingTalk connectors apply to their own JSON responses and sits far above any
// listing page the server returns, while keeping one request bounded.
const maxResponseBodyBytes int64 = 16 << 20

// errResponseBodyTooLarge reports a successful response the SDK refused to
// decode because its body exceeded maxResponseBodyBytes.
var errResponseBodyTooLarge = fmt.Errorf("response body exceeds the %d MiB limit", maxResponseBodyBytes>>20)

// countingReader counts the bytes a decoder pulls, so decodeResponseBody can
// tell "body too large" apart from "body is malformed JSON".
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// decodeResponseBody decodes a successful response envelope, refusing to read
// past maxResponseBodyBytes.
//
// The body is streamed through the counting reader rather than read into a
// buffer: the cap exists to bound memory, so buffering up to the cap first
// would defeat it for ordinary responses.
func decodeResponseBody(r io.Reader, target interface{}) error {
	limited := &countingReader{r: io.LimitReader(r, maxResponseBodyBytes+1)}
	if err := json.NewDecoder(limited).Decode(target); err != nil {
		if limited.n > maxResponseBodyBytes {
			return errResponseBodyTooLarge
		}
		return err
	}
	if limited.n > maxResponseBodyBytes {
		return errResponseBodyTooLarge
	}
	return nil
}

// extractServerCode pulls the server's structured error code from a JSON error
// body. Returns 0 when the body isn't JSON or carries no code.
//
// Two shapes are read: a bare {"code":1003,...} and the envelope the API
// actually sends, {"success":false,"error":{"code":1003,...}}. Without the
// second, APIError.Code was always 0 against this server, which silently
// disabled every "branch on the typed code" path documented above.
func extractServerCode(body []byte) int {
	var env struct {
		Code  int `json:"code"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return 0
	}
	if env.Code != 0 {
		return env.Code
	}
	return env.Error.Code
}

// parseResponse parses an HTTP response
func parseResponse(resp *http.Response, target interface{}) error {
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIErrorFromResponse(resp, readErrorBody(resp.Body))
	}

	if target == nil {
		return nil
	}

	return decodeResponseBody(resp.Body, target)
}
