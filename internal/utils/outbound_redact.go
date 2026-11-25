package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// redactedPlaceholder is what secret values are replaced with in logs and
// error strings.
const redactedPlaceholder = "***"

// sensitiveRequestQueryFields are the query parameters that must never reach a
// log line. ticket is the sandbox terminal's WebSocket handshake credential: a
// browser cannot set Authorization on an upgrade, so it travels in the query
// string and anyone holding it for its TTL can open a shell in the session's
// sandbox.
var sensitiveRequestQueryFields = map[string]struct{}{
	"access_token":          {},
	"authorization_attempt": {},
	"code":                  {},
	"expires":               {},
	"id_token":              {},
	"refresh_token":         {},
	"sig":                   {},
	"signature":             {},
	"state":                 {},
	"ticket":                {},
	"token":                 {},
}

// SanitizeQuery redacts credential-bearing parameters from a raw query string.
// Parsing the query (rather than string replacement) covers repeated,
// percent-encoded and differently-cased parameters. Invalid queries are
// omitted entirely instead of risk leaking a fragment.
//
// Shared by the access logger and the Langfuse trace middleware so both
// surfaces redact the same set.
func SanitizeQuery(raw string) string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[invalid query omitted]"
	}
	for key := range values {
		if _, sensitive := sensitiveRequestQueryFields[strings.ToLower(key)]; sensitive {
			values[key] = []string{redactedPlaceholder}
		}
	}
	return values.Encode()
}

// sensitiveOutboundQueryFields are query parameters that carry credentials to
// an outbound IM provider. Access tokens and corp secrets travel in the URL
// because the provider APIs require it, so any transport error (*url.Error)
// mirrors them into its message.
var sensitiveOutboundQueryFields = map[string]struct{}{
	"access_token":  {},
	"app_secret":    {},
	"appsecret":     {},
	"api_key":       {},
	"apikey":        {},
	"client_secret": {},
	"clientsecret":  {},
	"code":          {},
	"corpsecret":    {},
	"secret":        {},
	"secret_key":    {},
	"secretkey":     {},
	"session":       {},
	"signature":     {},
	"ticket":        {},
	"token":         {},
}

// RedactURLQuery replaces credential-bearing query values (and userinfo
// passwords) in rawURL with "***". Non-secret parameters are preserved so the
// error still identifies the endpoint.
func RedactURLQuery(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	changed := false
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), redactedPlaceholder)
			changed = true
		}
	}
	if parsed.RawQuery != "" {
		values, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			parsed.RawQuery = redactedPlaceholder
			return parsed.String()
		}
		for key := range values {
			if _, sensitive := sensitiveOutboundQueryFields[strings.ToLower(key)]; sensitive {
				values[key] = []string{redactedPlaceholder}
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = values.Encode()
		}
	}
	if !changed {
		return rawURL
	}
	return parsed.String()
}

// RedactRequestError redacts the URL embedded in any *url.Error in err's chain
// (http.Client wraps transport failures in one, and its message includes the
// full request URL). The original error is returned with its URL mutated, so
// callers keep using fmt.Errorf("...: %w", ...).
//
// Call it on the raw transport error before wrapping: fmt.Errorf renders its
// message immediately, so mutating an already-wrapped error cannot rewrite
// text that was already formatted.
func RedactRequestError(err error) error {
	if err == nil {
		return nil
	}
	// A redirect chain or a transport that itself returns a *url.Error can
	// nest more than one; redact every link, not just the outermost.
	for current := err; current != nil; current = errors.Unwrap(current) {
		urlErr, ok := current.(*url.Error)
		if !ok || urlErr == nil {
			continue
		}
		urlErr.URL = RedactURLQuery(urlErr.URL)
	}
	return err
}

// upstreamSummaryKeys are response fields safe to surface in an error: the
// provider's errcode/errmsg (WeCom) or code/message (DingTalk). Everything
// else in the body — debug echoes, request mirrors — is dropped.
var upstreamSummaryKeys = []string{"errcode", "errmsg", "code", "message", "msg"}

// upstreamSummaryValueLimit caps a single surfaced field; errmsg is upstream
// text and could itself be long or contain control characters.
const upstreamSummaryValueLimit = 200

// SummarizeUpstreamBody turns an outbound provider error body into a log-safe
// summary: the body length plus errcode/errmsg (or code/message) when the body
// is a JSON object. Raw bodies are never returned, so a gateway error page or
// a proxy echo cannot leak the request credentials or payload.
func SummarizeUpstreamBody(body []byte) string {
	summary := fmt.Sprintf("body_len=%d", len(body))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return summary
	}
	for _, key := range upstreamSummaryKeys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var text string
		quoted := true
		if err := json.Unmarshal(raw, &text); err != nil {
			// Numeric errcode values are common; keep their raw JSON token.
			text = string(raw)
			quoted = false
		}
		if text == "" {
			continue
		}
		text = SanitizeForLog(text)
		if len(text) > upstreamSummaryValueLimit {
			text = text[:upstreamSummaryValueLimit] + "..."
		}
		if quoted {
			summary += fmt.Sprintf(" %s=%q", key, text)
		} else {
			summary += fmt.Sprintf(" %s=%s", key, text)
		}
	}
	return summary
}
