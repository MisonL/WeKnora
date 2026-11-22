package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A 429's Retry-After reaches the caller as a number: the CLI's error envelope
// and any agent retry policy read it, so losing it here makes every consumer
// fall back to a guessed backoff.
func TestAPIErrorCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":1006,"message":"busy"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.ListKnowledgeBases(context.Background())
	if err == nil {
		t.Fatal("a 429 must be an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", apiErr.StatusCode)
	}
	if apiErr.RetryAfter != 42 {
		t.Fatalf("RetryAfter = %d, want 42", apiErr.RetryAfter)
	}
}

// A response without the header, or with the HTTP-date form, leaves the hint
// at zero so callers know they have to choose their own wait.
func TestParseRetryAfterIgnoresUnusableValues(t *testing.T) {
	for _, v := range []string{"", "  ", "not-a-number", "0", "-5", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		if got := parseRetryAfter(v); got != 0 {
			t.Fatalf("parseRetryAfter(%q) = %d, want 0", v, got)
		}
	}
	if got := parseRetryAfter(" 30 "); got != 30 {
		t.Fatalf("parseRetryAfter(\" 30 \") = %d, want 30", got)
	}
}

// TestUploadFailureSurfacesTheStoredRow is the SDK half of the enqueue-failure
// contract: the server answers 503 with a typed code and the row's identity in
// details, and the caller must be able to reach both without parsing prose.
func TestUploadFailureSurfacesTheStoredRow(t *testing.T) {
	const body = `{"success":false,"error":{"code":2500,` +
		`"message":"文件已保存，但解析任务未能入队，请稍后重新解析",` +
		`"details":{"knowledge_id":"kn-9","title":"a.pdf","parse_status":"failed"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	path := writeTempFile(t, "a.pdf", "payload")
	_, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", path, nil, nil, "", "", nil)
	if err == nil {
		t.Fatal("a stored-but-unqueued upload was reported as successful")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", apiErr.StatusCode)
	}
	if apiErr.Code != ServerErrKnowledgeEnqueueFailed {
		t.Fatalf("code = %d, want %d", apiErr.Code, ServerErrKnowledgeEnqueueFailed)
	}
	// The caller needs the row id to reparse it; without this the only way back
	// is to re-upload, which would store the same bytes as a second document
	// (the row is marked failed, and failed rows are exempt from dedup).
	if !strings.Contains(apiErr.Body, "kn-9") {
		t.Fatalf("the row id did not survive into the error body: %s", apiErr.Body)
	}
}
