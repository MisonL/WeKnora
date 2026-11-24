package router

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// One pass collects every knowledge a cancellable task references, across
// pages and states, and skips task types that are not per-knowledge.
func TestQueuedKnowledgeIDsCollectsAcrossPagesAndStates(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	asynqClient := asynq.NewClientFromRedisClient(redisClient)
	t.Cleanup(func() {
		_ = asynqClient.Close()
		_ = redisClient.Close()
	})
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}

	const pending = listPageSize + 5
	for i := 0; i < pending; i++ {
		enqueueTask(t, asynqClient, types.TypeDocumentProcess,
			fmt.Sprintf(`{"knowledge_id":"k-%d"}`, i), fmt.Sprintf("doc-%03d", i))
	}
	scheduleTask(t, asynqClient, types.TypeKnowledgePostProcess, `{"knowledge_id":"k-scheduled"}`, "post")
	enqueueTask(t, asynqClient, types.TypeKBClone, `{"knowledge_id":"k-kb-level"}`, "clone")

	ids, err := inspector.QueuedKnowledgeIDs(context.Background())
	if err != nil {
		t.Fatalf("QueuedKnowledgeIDs: %v", err)
	}
	if len(ids) != pending+1 {
		t.Fatalf("got %d ids, want %d", len(ids), pending+1)
	}
	for _, id := range []string{"k-0", fmt.Sprintf("k-%d", pending-1), "k-scheduled"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("missing %s", id)
		}
	}
	if _, ok := ids["k-kb-level"]; ok {
		t.Fatal("KB-level task types must not count")
	}
}

// A Redis failure is an error, not an empty queue.
func TestQueuedKnowledgeIDsReportsBackendErrors(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}
	server.Close()

	if _, err := inspector.QueuedKnowledgeIDs(context.Background()); err == nil {
		t.Fatal("expected an error when Redis is down")
	}
}

// The callers of this scan hold a reservation lock for at most a few seconds,
// so a caller that gave up must stop the scan instead of letting it run to its
// natural end over every queue and state.
func TestQueuedKnowledgeIDsHonoursTheCallersDeadline(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	asynqClient := asynq.NewClientFromRedisClient(redisClient)
	t.Cleanup(func() {
		_ = asynqClient.Close()
		_ = redisClient.Close()
	})
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}

	for i := 0; i < 3; i++ {
		enqueueTask(t, asynqClient, types.TypeDocumentProcess,
			fmt.Sprintf(`{"knowledge_id":"k-%d"}`, i), fmt.Sprintf("doc-%03d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's budget is already gone before the first page
	ids, err := inspector.QueuedKnowledgeIDs(ctx)
	if err == nil {
		t.Fatalf("a cancelled scan returned %d ids and no error", len(ids))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
}
