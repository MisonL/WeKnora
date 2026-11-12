package im

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// deadRedisClient returns a client that always fails (nothing listens there),
// used to exercise the fail-closed paths without stubbing the concrete type.
func deadRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 500 * time.Millisecond})
}

// recordingAdapter captures replies so tests can assert the user-facing
// fallback text without a real platform.
type recordingAdapter struct {
	mu      sync.Mutex
	replies []*ReplyMessage
}

func (a *recordingAdapter) Platform() Platform { return "test" }
func (a *recordingAdapter) VerifyCallback(*gin.Context) error {
	return nil
}
func (a *recordingAdapter) ParseCallback(*gin.Context) (*IncomingMessage, error) {
	return nil, nil
}
func (a *recordingAdapter) HandleURLVerification(*gin.Context) bool { return false }
func (a *recordingAdapter) SendReply(_ context.Context, _ *IncomingMessage, reply *ReplyMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies = append(a.replies, reply)
	return nil
}

func (a *recordingAdapter) allReplies() []*ReplyMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*ReplyMessage(nil), a.replies...)
}

func newTestRequest(userKey string, adapter Adapter) *qaRequest {
	ctx, cancel := context.WithCancel(context.Background())
	return &qaRequest{
		ctx:        ctx,
		cancel:     cancel,
		msg:        &IncomingMessage{UserID: "u1", Content: "hello"},
		adapter:    adapter,
		channel:    &IMChannel{},
		channelID:  "ch1",
		userKey:    userKey,
		enqueuedAt: time.Now(),
	}
}

// With Redis configured, a Redis failure while incrementing the per-user
// counter must reject the enqueue (fail-closed), matching the dedup semantics.
func TestRedisCheckAndIncrUserFailClosed(t *testing.T) {
	q := newQAQueue(1, 5, 3, 0, func(*qaRequest) {}, deadRedisClient())
	if err := q.redisCheckAndIncrUser(context.Background(), "ch1:u1:c1"); err == nil {
		t.Fatal("expected fail-closed error on Redis failure, got nil")
	}
}

func TestEnqueueFailClosedOnRedisError(t *testing.T) {
	q := newQAQueue(1, 5, 3, 0, func(*qaRequest) {}, deadRedisClient())
	adapter := &recordingAdapter{}
	pos, err := q.Enqueue(newTestRequest("ch1:u1:c1", adapter))
	if err == nil {
		t.Fatalf("expected enqueue to fail closed, got pos=%d", pos)
	}
	if !strings.Contains(err.Error(), "queue temporarily unavailable") {
		t.Fatalf("expected readable error, got %q", err.Error())
	}
}

// With Redis configured, a global-gate failure must not let the worker proceed.
func TestAcquireGlobalGateFailClosedOnRedisError(t *testing.T) {
	q := newQAQueue(1, 5, 3, 5, func(*qaRequest) {}, deadRedisClient())
	slot, err := q.acquireGlobalGate(context.Background())
	if err == nil {
		t.Fatalf("expected fail-closed error on gate Redis failure, got slot=%v", slot)
	}
	if slot {
		t.Fatal("gate must not report acquisition when Redis fails")
	}
}

// Gate without Redis (or with the limit disabled) stays open — single-instance
// mode relies on the local worker pool.
func TestAcquireGlobalGateNoRedisPasses(t *testing.T) {
	q := newQAQueue(1, 5, 3, 5, func(*qaRequest) {}, nil)
	if slot, err := q.acquireGlobalGate(context.Background()); err != nil || !slot {
		t.Fatalf("expected immediate acquisition without Redis, got slot=%v err=%v", slot, err)
	}
}

// Happy path with Redis: enqueue still works and the handler runs.
func TestEnqueueWithRedisProcessesRequest(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	processed := make(chan *qaRequest, 1)
	q := newQAQueue(1, 5, 3, 0, func(req *qaRequest) { processed <- req }, client)
	stopCh := make(chan struct{})
	q.Start(stopCh)
	defer close(stopCh)

	if _, err := q.Enqueue(newTestRequest("ch1:u1:c1", &recordingAdapter{})); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not invoked")
	}
}

// A panicking QA handler must not crash the process and must leave the user
// with the fixed fallback reply.
func TestWorkerPanicRecovered(t *testing.T) {
	q := newQAQueue(1, 5, 3, 0, func(*qaRequest) { panic("boom") }, nil)
	adapter := &recordingAdapter{}
	stopCh := make(chan struct{})
	q.Start(stopCh)
	defer close(stopCh)

	if _, err := q.Enqueue(newTestRequest("ch1:u1:c1", adapter)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		replies := adapter.allReplies()
		if len(replies) > 0 {
			if replies[0].Content != imErrorFallback {
				t.Fatalf("fallback reply = %q, want %q", replies[0].Content, imErrorFallback)
			}
			if !replies[0].IsFinal {
				t.Fatal("fallback reply must be final")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no fallback reply after panic")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Control commands must never be rate limited; resource commands must not
// bypass it.
func TestIsControlCommand(t *testing.T) {
	for _, name := range []string{"stop", "help", "clear", "STOP"} {
		if !isControlCommand(name) {
			t.Fatalf("isControlCommand(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"search", "info", ""} {
		if isControlCommand(name) {
			t.Fatalf("isControlCommand(%q) = true, want false", name)
		}
	}
}
