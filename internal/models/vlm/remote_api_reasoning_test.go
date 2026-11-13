package vlm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newVLMChatTestServer emulates an OpenAI-compatible chat completions endpoint
// and records the last decoded request body.
func newVLMChatTestServer(t *testing.T, lastRequest *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode VLM request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*lastRequest = req

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "content": "extracted text"}, "finish_reason": "stop"}
			]
		}`))
	}))
}

// testPNG is a minimal byte slice that http.DetectContentType reports as a PNG.
var testPNG = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16))

// TestRemoteAPIVLMSendsMaxCompletionTokensForReasoningModel is the regression
// test for issue #2537: GPT-5 / o-series vision models reject max_tokens and
// every non-default sampling parameter. The catalog marks those families on
// the OpenAI vendor, and the generic vendor carries the same family entries
// so relays that forward to OpenAI behave the same way.
func TestRemoteAPIVLMSendsMaxCompletionTokensForReasoningModel(t *testing.T) {
	withVLMSSRFWhitelist(t, "127.0.0.1")

	for _, provider := range []string{"openai", ""} {
		var lastRequest map[string]interface{}
		server := newVLMChatTestServer(t, &lastRequest)

		v, err := NewRemoteAPIVLM(&Config{
			BaseURL:   server.URL,
			ModelName: "gpt-5-nano",
			APIKey:    "sk-test",
			Provider:  provider,
		})
		if err != nil {
			t.Fatalf("NewRemoteAPIVLM: %v", err)
		}

		content, err := v.Predict(t.Context(), [][]byte{testPNG}, "extract the text")
		if err != nil {
			t.Fatalf("Predict (provider=%q): %v", provider, err)
		}
		if content != "extracted text" {
			t.Errorf("content = %q, want %q", content, "extracted text")
		}
		if _, ok := lastRequest["max_tokens"]; ok {
			t.Errorf("provider=%q: request carries max_tokens, which reasoning models reject: %v",
				provider, lastRequest["max_tokens"])
		}
		if got, ok := lastRequest["max_completion_tokens"]; !ok {
			t.Errorf("provider=%q: request is missing max_completion_tokens", provider)
		} else if got != float64(defaultMaxToks) {
			t.Errorf("max_completion_tokens = %v, want %d", got, defaultMaxToks)
		}
		if _, ok := lastRequest["temperature"]; ok {
			t.Errorf("provider=%q: request carries temperature, which reasoning models reject: %v",
				provider, lastRequest["temperature"])
		}
		server.Close()
	}
}

// TestRemoteAPIVLMKeepsMaxTokensForNonReasoningModel guards against the fix
// regressing ordinary vision models on an unknown (generic) endpoint, which
// still expect max_tokens.
func TestRemoteAPIVLMKeepsMaxTokensForNonReasoningModel(t *testing.T) {
	withVLMSSRFWhitelist(t, "127.0.0.1")

	var lastRequest map[string]interface{}
	server := newVLMChatTestServer(t, &lastRequest)
	defer server.Close()

	v, err := NewRemoteAPIVLM(&Config{
		BaseURL:   server.URL,
		ModelName: "qwen2.5-vl-7b-instruct",
		APIKey:    "sk-test",
	})
	if err != nil {
		t.Fatalf("NewRemoteAPIVLM: %v", err)
	}

	if _, err := v.Predict(t.Context(), [][]byte{testPNG}, "extract the text"); err != nil {
		t.Fatalf("Predict: %v", err)
	}

	if got, ok := lastRequest["max_tokens"]; !ok {
		t.Error("request is missing max_tokens")
	} else if got != float64(defaultMaxToks) {
		t.Errorf("max_tokens = %v, want %d", got, defaultMaxToks)
	}
	if _, ok := lastRequest["max_completion_tokens"]; ok {
		t.Error("request carries max_completion_tokens for a non-reasoning model")
	}
	if got, ok := lastRequest["temperature"]; !ok {
		t.Error("request is missing temperature")
	} else if f, isFloat := got.(float64); !isFloat || math.Abs(f-float64(defaultTemp)) > 1e-6 {
		t.Errorf("temperature = %v, want %v", got, defaultTemp)
	}
	messages := lastRequest["messages"].([]interface{})
	parts := messages[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("user content parts = %d, want text + image", len(parts))
	}
	image := parts[1].(map[string]interface{})["image_url"].(map[string]interface{})
	if !strings.HasPrefix(image["url"].(string), "data:image/png;base64,") {
		t.Errorf("image url = %q, want a PNG data URI", image["url"])
	}
}

// A completion stopped by the budget is never successful OCR. A prefix is
// carried on the error for an OCR caller that can use it, but a runaway fill
// of empty table cells is not text and must not ride along.
func TestRemoteAPIVLMReportsTruncatedCompletion(t *testing.T) {
	withVLMSSRFWhitelist(t, "127.0.0.1")
	for _, content := range []string{"", "partial extracted text", strings.Repeat("|  ", 2500)} {
		t.Run(fmt.Sprintf("chars=%d", len(content)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{
						"index": 0, "message": map[string]any{"role": "assistant", "content": content},
						"finish_reason": "length",
					}},
				})
			}))
			defer server.Close()
			v, err := NewRemoteAPIVLM(&Config{BaseURL: server.URL, ModelName: "generic-vision", Provider: "generic"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := v.Predict(t.Context(), [][]byte{testPNG}, "extract text")
			if !errors.Is(err, ErrTruncatedCompletion) {
				t.Fatalf("content=%q error=%v", got, err)
			}
			var trunc *TruncatedCompletionError
			if !errors.As(err, &trunc) {
				t.Fatalf("error %v does not carry the truncation detail", err)
			}
			if got != trunc.Partial {
				t.Errorf("returned content=%q, error carries %q", got, trunc.Partial)
			}
			if content == strings.Repeat("|  ", 2500) && strings.TrimSpace(got) != "" {
				t.Errorf("a fill of empty table cells was kept as text: %q", firstRunes(got, 40))
			}
			if strings.Contains(content, "partial extracted text") && !strings.Contains(got, "partial extracted text") {
				t.Errorf("a genuine prefix was dropped: %q", got)
			}
		})
	}
}

// firstRunes keeps failure messages readable when the value is thousands of
// repeated filler cells.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// TestPredictOptionsMaxTokensOverride pins the per-call budget: a caller whose
// output is sized by the image (OCR of a dense page) must be able to ask for
// more than the VLM_MAX_TOKENS default without changing every other call.
func TestPredictOptionsMaxTokensOverride(t *testing.T) {
	withVLMSSRFWhitelist(t, "127.0.0.1")
	t.Setenv("VLM_MAX_TOKENS", "6000")

	var lastRequest map[string]interface{}
	server := newVLMChatTestServer(t, &lastRequest)
	defer server.Close()

	v, err := NewRemoteAPIVLM(&Config{BaseURL: server.URL, ModelName: "qwen2.5-vl-7b-instruct", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("NewRemoteAPIVLM: %v", err)
	}
	tokens := 16000
	if _, err := v.PredictWithOptions(t.Context(), [][]byte{testPNG}, "transcribe", &PredictOptions{MaxTokens: &tokens}); err != nil {
		t.Fatalf("PredictWithOptions: %v", err)
	}
	if got := lastRequest["max_tokens"]; got != float64(16000) {
		t.Errorf("max_tokens = %v, want the per-call override 16000", got)
	}

	zero := 0
	lastRequest = nil
	if _, err := v.PredictWithOptions(t.Context(), [][]byte{testPNG}, "transcribe", &PredictOptions{MaxTokens: &zero}); err != nil {
		t.Fatalf("PredictWithOptions zero: %v", err)
	}
	if got := lastRequest["max_tokens"]; got != float64(6000) {
		t.Errorf("max_tokens = %v, want the VLM_MAX_TOKENS default when the override is not positive", got)
	}
}
