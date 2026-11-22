package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// miniredis is a single-threaded simulator: it cannot prove that SET NX and
// the Lua scripts stay atomic under real concurrent connections. These tests
// run only when pointed at a disposable Redis.
func newRealRedisService(t *testing.T) *knowledgeService {
	t.Helper()
	addr := os.Getenv("WEKNORA_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(context.Background()).Err())
	return &knowledgeService{redisClient: rdb}
}

func TestRealRedisImportLeaseAdmitsExactlyOneClaim(t *testing.T) {
	svc := newRealRedisService(t)
	ctx := context.Background()
	kbID := "real-kb-" + t.Name()
	t.Cleanup(func() { _ = svc.redisClient.Del(ctx, getFAQImportRunningKey(kbID)) })

	const n = 16
	var wg sync.WaitGroup
	successes := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info := &runningFAQImportInfo{
				TaskID: fmt.Sprintf("task-%d", i), EnqueuedAt: time.Now().Unix(),
				InstanceID: fmt.Sprintf("inst-%d", i),
			}
			if err := svc.claimFAQImportLease(ctx, kbID, info); err == nil {
				successes <- info.TaskID
			}
		}(i)
	}
	wg.Wait()
	close(successes)
	var got []string
	for id := range successes {
		got = append(got, id)
	}
	require.Len(t, got, 1, "SET NX has to admit exactly one of %d concurrent claims", n)
}

func TestRealRedisLeaseScriptsKeepOwnership(t *testing.T) {
	svc := newRealRedisService(t)
	ctx := context.Background()
	kbID := "real-kb-" + t.Name()
	t.Cleanup(func() { _ = svc.redisClient.Del(ctx, getFAQImportRunningKey(kbID)) })

	info := &runningFAQImportInfo{TaskID: "task-a", EnqueuedAt: time.Now().Unix(), InstanceID: "inst-a"}
	require.NoError(t, svc.claimFAQImportLease(ctx, kbID, info))

	ttl := svc.redisClient.TTL(ctx, getFAQImportRunningKey(kbID)).Val()
	require.True(t, ttl > faqImportLeaseTTL/2 && ttl <= faqImportLeaseTTL, "unexpected lease TTL %v", ttl)

	require.NoError(t, svc.renewFAQImportLease(ctx, kbID, "task-a", "inst-a"))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, kbID, "task-a", "inst-b"), errFAQImportLeaseTaken)
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, kbID, "task-b", "inst-a"), errFAQImportLeaseTaken)

	// A clear from the wrong instance leaves the marker in place.
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "task-a", "inst-b", info.EnqueuedAt))
	marker, err := svc.getRunningFAQImportInfo(ctx, kbID)
	require.NoError(t, err)
	require.NotNil(t, marker)
	require.Equal(t, "inst-a", marker.InstanceID)

	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "task-a", "inst-a", info.EnqueuedAt))
	marker, err = svc.getRunningFAQImportInfo(ctx, kbID)
	require.NoError(t, err)
	require.Nil(t, marker)
}
