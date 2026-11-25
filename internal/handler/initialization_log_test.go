package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
)

// The initialize endpoint receives plaintext model keys plus COS credentials.
// Its INFO line must stay useful for operators (which kb, which secrets were
// supplied) without ever containing the secret values themselves.
func TestInitializationLogSummaryOmitsSecrets(t *testing.T) {
	var req InitializationRequest
	body := `{
		"llm": {"source":"openai","modelName":"gpt","apiKey":"sk-llm-SECRET"},
		"embedding": {"source":"openai","modelName":"emb","apiKey":"sk-embed-SECRET"},
		"rerank": {"enabled":true,"modelName":"rr","apiKey":"sk-rerank-SECRET"},
		"multimodal": {"enabled":true,
			"vlm": {"modelName":"vlm","apiKey":"sk-vlm-SECRET"},
			"storageType":"cos",
			"cos": {"secretId":"cos-id-SECRET","secretKey":"cos-key-SECRET","region":"ap-x"}},
		"documentSplitting": {"chunkSize":512,"separators":["\n"]}
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })
	logger.SetLogLevel(logger.LevelInfo)

	logInitializationRequestSummary(context.Background(), "kb-123", &req)

	out := buf.String()
	if !strings.Contains(out, "kb-123") {
		t.Fatalf("log line lost the kb id: %q", out)
	}
	for _, want := range []string{
		"llmApiKeyConfigured: true",
		"embeddingApiKeyConfigured: true",
		"rerankApiKeyConfigured: true",
		"vlmApiKeyConfigured: true",
		"cosSecretIdConfigured: true",
		"cosSecretKeyConfigured: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line missing %q: %q", want, out)
		}
	}
	for _, secret := range []string{
		"sk-llm-SECRET",
		"sk-embed-SECRET",
		"sk-rerank-SECRET",
		"sk-vlm-SECRET",
		"cos-id-SECRET",
		"cos-key-SECRET",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("log line leaked %q: %q", secret, out)
		}
	}
}

func TestInitializationLogSummaryReportsEmptySecrets(t *testing.T) {
	var req InitializationRequest
	if err := json.Unmarshal([]byte(`{"llm":{"source":"local","modelName":"m"}}`), &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })
	logger.SetLogLevel(logger.LevelInfo)

	logInitializationRequestSummary(context.Background(), "kb-9", &req)

	out := buf.String()
	if strings.Contains(out, "llmApiKeyConfigured: true") {
		t.Fatalf("an omitted key must not be reported as configured: %q", out)
	}
	if !strings.Contains(out, "llmApiKeyConfigured: false") {
		t.Fatalf("missing configured=false for an omitted key: %q", out)
	}
}
