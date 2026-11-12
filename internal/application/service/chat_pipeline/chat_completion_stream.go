package chatpipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

// EmptyTruncatedAnswerFallback explains an output-budget exhaustion with no answer text.
const EmptyTruncatedAnswerFallback = "Sorry, this answer hit the model's per-response output limit " +
	"before any text was produced. Try narrowing the question, or raise max_completion_tokens."

const (
	// maxStreamStartRetries is the number of retries after the first attempt
	// when opening the stream fails transiently. Once the stream is open the
	// consume loop has its own, smaller budget.
	maxStreamStartRetries = 2
	// maxPrematureStreamRetries is the number of times a stream that failed
	// before emitting any content is restarted. A stream that already emitted
	// content is never retried: the user would see it twice.
	maxPrematureStreamRetries = 1
	// streamRetryBaseDelay is the wait before the first retry; it doubles per
	// attempt (500ms, 1s).
	streamRetryBaseDelay = 500 * time.Millisecond

	// defaultStreamStallTimeout bounds the silence between two stream events.
	// It is a stall budget, not a total budget: a stream that keeps producing
	// may run for the full provider transport timeout.
	defaultStreamStallTimeout = 120 * time.Second
	// streamStallTimeoutEnv overrides defaultStreamStallTimeout. It accepts a
	// Go duration ("90s", "2m") or a plain number of seconds; invalid values
	// fall back to the default.
	streamStallTimeoutEnv = "WEKNORA_LLM_STREAM_STALL_TIMEOUT"
)

// streamTransientStatusPattern matches a retryable HTTP status as a whole
// number, so "max_tokens 5000" or a request id containing 429 is not read as
// a status.
var streamTransientStatusPattern = regexp.MustCompile(`\b(408|429|500|502|503|504|529)\b`)

// streamTransientErrorMarkers are substrings of transient failures that reach
// this layer as text: the stream reader reports them on an error chunk, and
// partial streams close with StreamEndedEarlyError instead of a typed error.
var streamTransientErrorMarkers = []string{
	"rate limit",
	"overloaded", "timeout", "timed out",
	"connection", "server error", "temporarily unavailable",
	"deadline exceeded", "stalled", "unexpected eof",
	strings.ToLower(types.StreamEndedEarlyError),
}

// isTransientStreamError mirrors internal/agent's classifier: typed HTTP and
// transport errors decide on their own, and only then does the message text
// get matched, so a 400 whose body mentions "timeout" is not retried.
func isTransientStreamError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		code := httpErr.StatusCode
		return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests ||
			code >= http.StatusInternalServerError
	}
	var transportErr *api.TransportError
	if errors.As(err, &transportErr) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	if streamTransientStatusPattern.MatchString(errStr) {
		return true
	}
	for _, marker := range streamTransientErrorMarkers {
		if strings.Contains(errStr, marker) {
			return true
		}
	}
	return false
}

// streamStallTimeout returns how long a stream may produce no event before it
// is reported as stalled.
func streamStallTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(streamStallTimeoutEnv))
	if raw == "" {
		return defaultStreamStallTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultStreamStallTimeout
}

// chatStreamAttempt is one opened stream plus the cancel that releases the
// provider connection when the attempt is abandoned (retry, stall, consumer
// gone). Without it an abandoned stream holds its body open until the
// transport timeout expires.
type chatStreamAttempt struct {
	responses <-chan types.StreamResponse
	cancel    context.CancelFunc
}

// openChatStream opens a single stream attempt. The caller (startChatStream)
// owns the retry budget, so the transport-level retry is disabled here to keep
// the two from multiplying.
func openChatStream(ctx context.Context, chatModel chat.Chat,
	messages []chat.Message, opt *chat.ChatOptions,
) (*chatStreamAttempt, error) {
	streamCtx, cancel := context.WithCancel(api.WithoutTransportRetry(ctx))
	responses, err := chatModel.ChatStream(streamCtx, messages, opt)
	if err != nil {
		cancel()
		return nil, err
	}
	if responses == nil {
		cancel()
		return nil, errors.New("chat stream returned nil channel")
	}
	return &chatStreamAttempt{responses: responses, cancel: cancel}, nil
}

// startChatStream opens the stream, retrying transient failures of the
// opening handshake with exponential backoff. Only the opening is retried:
// once the stream has emitted content a retry would duplicate it.
func startChatStream(ctx context.Context, chatModel chat.Chat,
	messages []chat.Message, opt *chat.ChatOptions, sessionID string,
) (*chatStreamAttempt, error) {
	var lastErr error
	for attempt := 0; attempt <= maxStreamStartRetries; attempt++ {
		if attempt > 0 {
			delay := streamRetryBaseDelay << uint(attempt-1)
			pipelineWarn(ctx, "Stream", "model_call_retry", map[string]interface{}{
				"session_id": sessionID,
				"attempt":    attempt,
				"max":        maxStreamStartRetries,
				"retry_in":   delay.String(),
				"error":      api.TruncateForDebug(lastErr.Error(), 300),
			})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		attemptStream, err := openChatStream(ctx, chatModel, messages, opt)
		if err == nil {
			return attemptStream, nil
		}
		if !isTransientStreamError(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// stalledStreamMessage is the user-visible error emitted when a stream stops
// producing events.
func stalledStreamMessage(timeout time.Duration) string {
	return fmt.Sprintf("the model stopped responding (no output for %s); the answer was interrupted", timeout)
}

// IsLengthFinishReason reports whether a provider ended a response because its
// completion-token budget was exhausted.
func IsLengthFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	default:
		return false
	}
}

// PluginChatCompletionStream implements streaming chat completion functionality
// as a plugin that can be registered to EventManager
type PluginChatCompletionStream struct {
	modelService interfaces.ModelService // Interface for model operations
}

// NewPluginChatCompletionStream creates a new PluginChatCompletionStream instance
// and registers it with the EventManager
func NewPluginChatCompletionStream(eventManager *EventManager,
	modelService interfaces.ModelService,
) *PluginChatCompletionStream {
	res := &PluginChatCompletionStream{
		modelService: modelService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginChatCompletionStream) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHAT_COMPLETION_STREAM}
}

// OnEvent handles streaming chat completion events
// It prepares the chat model, messages, and initiates streaming response
func (p *PluginChatCompletionStream) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "Stream", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"user_question":  chatManage.UserContent,
		"history_rounds": len(chatManage.History),
		"chat_model":     chatManage.ChatModelID,
	})

	// Prepare chat model and options
	chatModel, opt, err := prepareChatModel(ctx, p.modelService, chatManage)
	if err != nil {
		return ErrGetChatModel.WithError(err)
	}

	// Prepare base messages without history

	chatMessages, modelContext := prepareMessagesWithModelContext(ctx, chatManage)
	chatMessages = modelContext.EncodeMessages(chatMessages)
	reportModelContextLeaks(ctx, "Stream", modelContext, chatMessages)
	ctx = withPromptCacheMetadata(ctx, chatModel, chatMessages, opt, "knowledge_qa")
	pipelineInfo(ctx, "Stream", "messages_ready", map[string]interface{}{
		"message_count": len(chatMessages),
		"system_prompt": chatMessages[0].Content,
	})
	pipelineInfo(ctx, "Stream", "user_message", map[string]interface{}{
		"content": chatMessages[len(chatMessages)-1].Content,
	})
	// EventBus is required for event-driven streaming
	if chatManage.EventBus == nil {
		pipelineError(ctx, "Stream", "eventbus_missing", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return ErrModelCall.WithError(errors.New("EventBus is required for streaming"))
	}
	eventBus := chatManage.EventBus

	pipelineInfo(ctx, "Stream", "eventbus_ready", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})

	// Initiate streaming chat model call with independent context
	pipelineInfo(ctx, "Stream", "model_call", map[string]interface{}{
		"chat_model": chatManage.ChatModelID,
	})
	stream, err := startChatStream(ctx, chatModel, chatMessages, opt, chatManage.SessionID)
	if err != nil {
		pipelineError(ctx, "Stream", "model_call", map[string]interface{}{
			"chat_model": chatManage.ChatModelID,
			"error":      err.Error(),
		})
		return ErrModelCall.WithError(err)
	}

	pipelineInfo(ctx, "Stream", "model_started", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})

	// Start goroutine to consume channel and emit events directly.
	// reasoning_content is routed to EventAgentThought (SSE response_type=thinking)
	// and plain answer text to EventAgentFinalAnswer, matching the Agent pipeline.
	// The goroutine monitors ctx.Done() to avoid leaking when the context is cancelled
	// and the upstream channel is not closed promptly.
	go func() {
		current := stream
		defer func() { current.cancel() }()

		answerDecoder := modelContext.StreamDecoder()
		thinkingDecoder := modelContext.StreamDecoder()
		thinkingID := fmt.Sprintf("%s-thinking", uuid.New().String()[:8])
		answerID := fmt.Sprintf("%s-answer", uuid.New().String()[:8])
		thinkingOpen := false
		answerCompleted := false
		answerProduced := false
		contentEmitted := false
		prematureRetries := 0

		stallTimeout := streamStallTimeout()
		stallTimer := time.NewTimer(stallTimeout)
		defer stallTimer.Stop()
		resetStallTimer := func() {
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallTimer.Reset(stallTimeout)
		}

		closeThinking := func() {
			if !thinkingOpen {
				return
			}
			eventBus.Emit(ctx, types.Event{
				ID:        thinkingID,
				Type:      types.EventType(event.EventAgentThought),
				SessionID: chatManage.SessionID,
				Data: event.AgentThoughtData{
					Done: true,
				},
			})
			thinkingOpen = false
		}

		// flushDecoders drains any handle suffix the stream decoders held back to
		// bridge references split across provider chunks. Both the normal close
		// and the cancellation path must call this, otherwise a resource
		// reference in flight at teardown is silently dropped (and never
		// persisted, since the assistant message is saved from these events).
		flushDecoders := func() {
			thinkingTail := thinkingDecoder.Flush()
			if thinkingTail != "" {
				_ = eventBus.Emit(ctx, types.Event{
					ID:        thinkingID,
					Type:      types.EventType(event.EventAgentThought),
					SessionID: chatManage.SessionID,
					Data:      event.AgentThoughtData{Content: thinkingTail},
				})
			}
			answerTail := answerDecoder.Flush()
			if answerTail != "" {
				_ = eventBus.Emit(ctx, types.Event{
					ID:        answerID,
					Type:      types.EventType(event.EventAgentFinalAnswer),
					SessionID: chatManage.SessionID,
					Data:      event.AgentFinalAnswerData{Content: answerTail},
				})
			}
		}

		for {
			select {
			case <-ctx.Done():
				flushDecoders()
				closeThinking()
				pipelineInfo(ctx, "Stream", "context_cancelled", map[string]interface{}{
					"session_id": chatManage.SessionID,
				})
				return

			case <-stallTimer.C:
				message := stalledStreamMessage(stallTimeout)
				pipelineError(ctx, "Stream", "stream_stall", map[string]interface{}{
					"session_id":    chatManage.SessionID,
					"stall_timeout": stallTimeout.String(),
				})
				eventBus.Emit(ctx, types.Event{
					ID:        fmt.Sprintf("%s-error", uuid.New().String()[:8]),
					Type:      types.EventType(event.EventError),
					SessionID: chatManage.SessionID,
					Data: event.ErrorData{
						Error:     message,
						Stage:     "chat_completion_stream",
						SessionID: chatManage.SessionID,
					},
				})
				current.cancel()
				closeThinking()
				return

			case response, ok := <-current.responses:
				if !ok {
					flushDecoders()
					closeThinking()
					pipelineInfo(ctx, "Stream", "channel_close", map[string]interface{}{
						"session_id": chatManage.SessionID,
					})
					return
				}
				resetStallTimer()

				// A stream that stopped without a finish reason is a broken
				// one, not a short answer: report it the way a read error is
				// reported instead of closing the answer as complete.
				if response.ResponseType == types.ResponseTypeAnswer && response.Done &&
					response.FinishReason == types.FinishReasonIncomplete {
					response.ResponseType = types.ResponseTypeError
					response.Content = types.StreamEndedEarlyError
				}
				if response.ResponseType == types.ResponseTypeError {
					// A failure before any content reached the user can be
					// retried as a whole call; once content was emitted a
					// retry would replay what the user already saw.
					if !contentEmitted && prematureRetries < maxPrematureStreamRetries &&
						isTransientStreamError(errors.New(response.Content)) {
						prematureRetries++
						pipelineWarn(ctx, "Stream", "stream_retry", map[string]interface{}{
							"session_id": chatManage.SessionID,
							"attempt":    prematureRetries,
							"max":        maxPrematureStreamRetries,
							"error":      api.TruncateForDebug(response.Content, 300),
						})
						current.cancel()
						retryStream, retryErr := startChatStream(
							ctx, chatModel, chatMessages, opt, chatManage.SessionID,
						)
						if retryErr == nil {
							current = retryStream
							answerDecoder = modelContext.StreamDecoder()
							thinkingDecoder = modelContext.StreamDecoder()
							// The previous attempt emitted no content, so any
							// terminal marker it produced must not suppress the
							// retried answer.
							answerCompleted = false
							answerProduced = false
							resetStallTimer()
							continue
						}
						pipelineError(ctx, "Stream", "stream_retry_failed", map[string]interface{}{
							"session_id": chatManage.SessionID,
							"error":      api.TruncateForDebug(retryErr.Error(), 300),
						})
					}
					pipelineError(ctx, "Stream", "stream_error", map[string]interface{}{
						"session_id": chatManage.SessionID,
						"error":      response.Content,
					})
					eventBus.Emit(ctx, types.Event{
						ID:        fmt.Sprintf("%s-error", uuid.New().String()[:8]),
						Type:      types.EventType(event.EventError),
						SessionID: chatManage.SessionID,
						Data: event.ErrorData{
							Error:     response.Content,
							Stage:     "chat_completion_stream",
							SessionID: chatManage.SessionID,
						},
					})
					// 错误块是这条流的终态：继续等待只会让失败后挂到 stall
					// 超时，或让上游在错误之后再补发一个完整回答，用户就会
					// 同时看到“报错”和“正常答案”。与 stall 分支同样处理。
					current.cancel()
					closeThinking()
					return
				}

				if response.ResponseType == types.ResponseTypeThinking {
					response.Content = thinkingDecoder.Feed(response.Content)
					if response.Done {
						response.Content += thinkingDecoder.Flush()
					}
					if response.Content != "" {
						thinkingOpen = true
						contentEmitted = true
						eventBus.Emit(ctx, types.Event{
							ID:        thinkingID,
							Type:      types.EventType(event.EventAgentThought),
							SessionID: chatManage.SessionID,
							Data: event.AgentThoughtData{
								Content: response.Content,
								Done:    false,
							},
						})
					}
					if response.Done {
						closeThinking()
					}
					continue
				}

				if response.ResponseType == types.ResponseTypeAnswer {
					// Providers can emit a completion once for finish_reason and again
					// for their EOF sentinel. A final answer is a terminal event for a
					// single stream, so forwarding a later duplicate would put an answer
					// after the session's complete event.
					if answerCompleted {
						continue
					}
					response.Content = answerDecoder.Feed(response.Content)
					if response.Done {
						response.Content += answerDecoder.Flush()
						answerCompleted = true
					}
					if strings.TrimSpace(response.Content) != "" {
						answerProduced = true
					}
					truncated := response.Done && IsLengthFinishReason(response.FinishReason)
					if truncated && !answerProduced {
						response.Content = EmptyTruncatedAnswerFallback
						answerProduced = true
					}
					if response.Content != "" {
						contentEmitted = true
					}
					closeThinking()
					eventBus.Emit(ctx, types.Event{
						ID:        answerID,
						Type:      types.EventType(event.EventAgentFinalAnswer),
						SessionID: chatManage.SessionID,
						Data: event.AgentFinalAnswerData{
							Content:   response.Content,
							Done:      response.Done,
							Truncated: truncated,
						},
					})
				}
			}
		}
	}()

	return next()
}
