package vlm

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// defaultTimeout is the fallback HTTP timeout for a single VLM request.
	// Dense scanned-PDF OCR (full-page text + layout extraction) can take well
	// over a minute on slow endpoints, so this is intentionally generous and
	// can be raised further via VLM_HTTP_TIMEOUT_SECONDS.
	defaultTimeout = 180 * time.Second
	// defaultMaxToks is the fallback completion budget for a single VLM
	// request. Reasoning vision models spend part of it on hidden thinking
	// tokens, so a dense scanned page can stop at finish_reason=length; raise
	// it via VLM_MAX_TOKENS.
	defaultMaxToks = 5000
	defaultTemp    = 0.1
)

// vlmHTTPTimeout returns the HTTP client timeout for VLM requests, read from
// the VLM_HTTP_TIMEOUT_SECONDS env var when set (and positive), falling back to
// defaultTimeout otherwise. Shared by all OpenAI-compatible VLM backends.
func vlmHTTPTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("VLM_HTTP_TIMEOUT_SECONDS")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultTimeout
}

// vlmMaxTokens returns the completion budget for a single VLM request, read
// from the VLM_MAX_TOKENS env var when set (and positive), falling back to
// defaultMaxToks otherwise. The budget covers thinking tokens as well, so a
// reasoning vision model can exhaust the default on a dense page and fail OCR
// with finish_reason=length; this makes the cap adjustable without a rebuild.
func vlmMaxTokens() int {
	if v := strings.TrimSpace(os.Getenv("VLM_MAX_TOKENS")); v != "" {
		if tokens, err := strconv.Atoi(v); err == nil && tokens > 0 {
			return tokens
		}
	}
	return defaultMaxToks
}

// RemoteAPIVLM implements VLM on top of the catalog-driven chat client: a
// vision call is a chat call whose user turn carries images, so the vendor
// dialect (max_tokens field, sampling restrictions, protocol) is resolved
// exactly like it is for chat models.
type RemoteAPIVLM struct {
	modelName   string
	modelID     string
	chat        chat.Chat
	temperature float64
}

// NewRemoteAPIVLM creates a remote-API backed VLM instance.
func NewRemoteAPIVLM(config *Config) (*RemoteAPIVLM, error) {
	if err := validateVLMBaseURL(config.BaseURL); err != nil {
		return nil, err
	}

	temp := float64(defaultTemp)
	extra := make(map[string]string, len(config.Extra))
	for k, v := range config.Extra {
		if s, ok := v.(string); ok {
			extra[k] = s
		}
	}
	if raw, ok := extra["temperature"]; ok {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			temp = f
		}
	}

	client, err := chat.NewRemoteChat(&chat.ChatConfig{
		Source:        types.ModelSourceRemote,
		BaseURL:       config.BaseURL,
		ModelName:     config.ModelName,
		APIKey:        config.APIKey,
		ModelID:       config.ModelID,
		Provider:      config.Provider,
		ExtraConfig:   extra,
		CustomHeaders: config.CustomHeaders,
		AppID:         config.AppID,
		AppSecret:     config.AppSecret,
		Spec:          config.Spec,
	})
	if err != nil {
		return nil, err
	}
	return &RemoteAPIVLM{
		modelName:   config.ModelName,
		modelID:     config.ModelID,
		chat:        client,
		temperature: temp,
	}, nil
}

// Predict sends images with a text prompt through the chat client.
func (v *RemoteAPIVLM) Predict(ctx context.Context, imgBytesList [][]byte, prompt string) (string, error) {
	return v.PredictWithOptions(ctx, imgBytesList, prompt, nil)
}

// PredictWithOptions sends the same request, carrying a per-call thinking
// switch into the chat client. The switch rides on Options.Thinking, which the
// protocol layer turns into whatever key this vendor uses, so a model that only
// understands chat_template_kwargs still gets that spelling rather than a
// bare enable_thinking the server would ignore.
func (v *RemoteAPIVLM) PredictWithOptions(
	ctx context.Context, imgBytesList [][]byte, prompt string, opts *PredictOptions,
) (string, error) {
	parts := []chat.MessageContentPart{{Type: "text", Text: prompt}}
	totalImageSize := 0
	for _, imgBytes := range imgBytesList {
		if len(imgBytes) == 0 {
			continue
		}
		totalImageSize += len(imgBytes)
		dataURI := fmt.Sprintf("data:%s;base64,%s",
			detectImageMIME(imgBytes), base64.StdEncoding.EncodeToString(imgBytes))
		parts = append(parts, chat.MessageContentPart{
			Type: "image_url", ImageURL: &chat.ImageURL{URL: dataURI, Detail: "auto"},
		})
	}
	logger.Infof(ctx, "[VLM] Calling chat protocol, model=%s, numImages=%d, totalImageSize=%d",
		v.modelName, len(imgBytesList), totalImageSize)

	maxTokens := vlmMaxTokens()
	if opts != nil && opts.MaxTokens != nil && *opts.MaxTokens > 0 {
		maxTokens = *opts.MaxTokens
	}
	chatOpts := &chat.ChatOptions{
		Temperature: v.temperature,
		MaxTokens:   maxTokens,
	}
	// Only an explicit preference is forwarded. The protocol layer skips the
	// thinking fields entirely when nobody asked for a level, which is what
	// leaves an always-on reasoning model running as its vendor intends.
	if opts != nil && opts.Thinking != nil {
		chatOpts.Thinking = opts.Thinking
	}

	ctx, cancel := context.WithTimeout(ctx, vlmHTTPTimeout())
	defer cancel()
	resp, err := v.chat.Chat(ctx, []chat.Message{{Role: "user", MultiContent: parts}}, chatOpts)
	if err != nil {
		return "", fmt.Errorf("VLM request: %w", err)
	}
	content := resp.Content
	if resp.FinishReason == "length" {
		// A partial transcription is still useful to an OCR caller, so it
		// rides along on the error instead of being dropped: the caller
		// decides whether an incomplete page is worth indexing. It is still
		// an error so a caller that needs a whole answer cannot mistake a
		// cut-off completion for a successful one.
		// The caller gets the usable prefix, never the raw loop tail: a page
		// that was cut is still worth indexing, a decoding loop is not.
		partial := usablePrefix(content)
		return partial, &TruncatedCompletionError{MaxTokens: maxTokens, Partial: partial}
	}
	logger.Infof(ctx, "[VLM] response received, len=%d", len(content))
	return content, nil
}

func (v *RemoteAPIVLM) GetModelName() string { return v.modelName }
func (v *RemoteAPIVLM) GetModelID() string   { return v.modelID }

// detectImageMIME returns the MIME type for the given image bytes.
func detectImageMIME(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "image/png"
}

// usablePrefix trims a cut-off completion down to transcribable text. A model
// that exhausts its budget on a decoding loop returns a long fill of empty
// table cells; handing that to the caller as "partial text" would index
// thousands of blank rows as though they said something. Cut the repeated
// tail, then refuse anything without a letter or a digit.
func usablePrefix(content string) string {
	content = strings.TrimSpace(cutRepeatedTail(content))
	if !strings.ContainsFunc(content, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return ""
	}
	return content
}

// cutRepeatedTail shortens content whose tail is the same few-token phrase
// repeated back-to-back, which is what a model does once it starts looping.
// A human-transcribed page does repeat structure (table rows, menu items), so
// only a tail longer than repeatedTailBudget is shortened, and it is cut at
// the start of the run rather than rewritten.
func cutRepeatedTail(content string) string {
	const (
		minPeriod    = 2
		maxPeriod    = 40
		repeatsToCut = 8
	)
	runes := []rune(content)
	n := len(runes)
	for period := minPeriod; period <= maxPeriod && period*repeatsToCut <= n; period++ {
		// Count how many times the final `period`-rune unit repeats backwards.
		reps := 1
		for start := n - period*2; start >= 0 && string(runes[start:start+period]) == string(runes[n-period:n]); start -= period {
			reps++
		}
		if reps >= repeatsToCut {
			return string(runes[:n-period*reps])
		}
	}
	return content
}
