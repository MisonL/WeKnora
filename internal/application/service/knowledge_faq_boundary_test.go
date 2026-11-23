package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This file closes the verification gaps that miniredis and the sqlite test
// schema cannot cover: the FAQ result row's persistence semantics on real
// PostgreSQL, the marker-protocol compatibility that decides whether a
// rollback mid-import is safe, and how much clock skew the freshness fence can
// absorb. Everything is gated on disposable stores: the tests skip when
// WEKNORA_TEST_POSTGRES_DSN / WEKNORA_TEST_REDIS_ADDR are absent.

func boundaryPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("WEKNORA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not set")
	}
	require.Contains(t, dsn, "weknora_boundary_verify", "use the disposable boundary database")
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	base, err := gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	schema := fmt.Sprintf("boundary_%d", time.Now().UnixNano())
	require.NoError(t, base.Exec("CREATE SCHEMA "+schema).Error)
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), config)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Knowledge{}))
	t.Cleanup(func() {
		_ = base.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if connection, err := base.DB(); err == nil {
			_ = connection.Close()
		}
	})
	return db
}

// The persisted FAQ result row is what every later stale-replace fence reads.
// UpdateKnowledge is GORM's Save: a whole-row write with no version guard, so
// two runs writing the row concurrently lose one update -- the final row is
// whichever writer landed last, including fields the other run set. This test
// demonstrates the exposure on production PostgreSQL, and that the fence still
// holds whichever result landed: a replace retry enqueued before it is refused.
func TestFAQResultRowPersistenceSemantics(t *testing.T) {
	db := boundaryPostgres(t)
	repo := repository.NewKnowledgeRepository(db)
	const tenantID = uint64(7)
	ctx := context.Background()

	seed := &types.Knowledge{ID: "faq", TenantID: tenantID, KnowledgeBaseID: "kb",
		Type: types.KnowledgeTypeFAQ, CustomMetadata: types.JSON("[]")}
	require.NoError(t, seed.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID: "old", ImportedAt: time.Now().Add(-time.Hour),
	}))
	require.NoError(t, repo.UpdateKnowledge(ctx, seed))

	// Two runs read the row at the same instant (as two writers that passed
	// the lease gate together would) and each saves back its own row.
	base := time.Now().Truncate(time.Second)
	var wg sync.WaitGroup
	for i, task := range []string{"task-a", "task-b"} {
		wg.Add(1)
		go func(i int, taskID string) {
			defer wg.Done()
			row, err := repo.GetKnowledgeByID(ctx, tenantID, "faq")
			require.NoError(t, err)
			importedAt := base.Add(time.Duration(i) * time.Second)
			require.NoError(t, row.SetLastFAQImportResult(&types.FAQImportResult{
				TaskID: taskID, ImportedAt: importedAt, ImportedAtMillis: importedAt.UnixMilli(),
			}))
			// Deterministic interleave: both reads precede both writes, which
			// is exactly the lost-update shape.
			time.Sleep(time.Duration(i) * 100 * time.Millisecond)
			require.NoError(t, repo.UpdateKnowledge(ctx, row))
		}(i, task)
	}
	wg.Wait()

	final, err := repo.GetKnowledgeByID(ctx, tenantID, "faq")
	require.NoError(t, err)
	result, err := final.GetLastFAQImportResult()
	require.NoError(t, err)
	require.NotNil(t, result)
	t.Logf("winner of the unguarded whole-row write: task=%s imported_at=%s (the other result was lost with no trace)",
		result.TaskID, result.ImportedAt)
	require.Contains(t, []string{"task-a", "task-b"}, result.TaskID)

	// The fence direction does not depend on which writer won.
	older := &types.FAQImportPayload{Mode: types.FAQBatchModeReplace,
		EnqueuedAt: result.ImportedAt.Unix() - 1, EnqueuedAtMillis: result.ImportedAt.UnixMilli() - 1000}
	require.True(t, faqImportRetryIsStale(older, result))
	newer := &types.FAQImportPayload{Mode: types.FAQBatchModeReplace,
		EnqueuedAt: result.ImportedAt.Unix() + 1, EnqueuedAtMillis: result.ImportedAt.UnixMilli() + 1000}
	require.False(t, faqImportRetryIsStale(newer, result))
}

// A rollback must keep old and new processes safe in the same Redis. Verified
// on a real server: bare task-id markers (the oldest form) keep their
// task-id identity under the new scripts; a modern instance-bound marker is
// renewed and cleared only by its own instance, so an old process -- which
// never sends an instance id -- cannot drive it; and admission stays exclusive
// in both directions, which is what the lease gate needs during a mixed fleet.
func TestLegacyRollbackProtocolCompatibility(t *testing.T) {
	svc := newRealRedisService(t)
	ctx := context.Background()
	kbID := "real-kb-" + t.Name()
	key := getFAQImportRunningKey(kbID)
	t.Cleanup(func() { _ = svc.redisClient.Del(ctx, key) })

	// (1) Bare task-id marker: the new renew refreshes it for its own task and
	// refuses another; the new clear honours it; a new claim is refused while
	// it exists, so a rolled-back submission keeps the KB reserved from the
	// new code.
	require.NoError(t, svc.redisClient.Set(ctx, key, "legacy-task", 3*time.Hour).Err())
	require.NoError(t, svc.renewFAQImportLease(ctx, kbID, "legacy-task", "", 0))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, kbID, "other-task", "inst-other", 0),
		errFAQImportLeaseTaken)
	var held *faqImportLeaseHeldError
	require.ErrorAs(t, svc.claimFAQImportLease(ctx, kbID, leaseInfo("modern")), &held)
	require.Equal(t, "legacy-task", held.TaskID)
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "legacy-task", "", 0))

	// (2) A modern marker: an old-style caller (no instance id, matching task
	// id) may neither renew nor clear it -- the instance rule holds in both
	// directions, so a rolled-back worker cannot extend or orphan a marker the
	// new version owns.
	require.NoError(t, svc.claimFAQImportLease(ctx, kbID, leaseInfo("modern")))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, kbID, "modern", "", 0), errFAQImportLeaseTaken)
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "modern", "", 0))
	marker, err := svc.getRunningFAQImportInfo(ctx, kbID)
	require.NoError(t, err)
	require.NotNil(t, marker, "an old-style conditional clear must not remove a modern marker")

	// (3) A stale *new-version* retry (its lease lapsed, a newer task holds
	// the key) cannot renew, reclaim or clear the newer task's marker -- the
	// rules that make a mixed fleet safe are the same ones the rollback relies
	// on.
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "modern", "inst-modern", 0))
	require.NoError(t, svc.claimFAQImportLease(ctx, kbID, leaseInfo("newer")))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, kbID, "stale-retry", "inst-stale", 0),
		errFAQImportLeaseTaken)
	require.ErrorIs(t, svc.renewFAQImportLeaseAllow(ctx, kbID, "stale-retry", "inst-stale", 0, true),
		errFAQImportLeaseTaken, "reclaim must not fire beside a live marker")
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, kbID, "stale-retry", "inst-stale", 0))
	marker, err = svc.getRunningFAQImportInfo(ctx, kbID)
	require.NoError(t, err)
	require.Equal(t, "newer", marker.TaskID)
}

// The freshness fence compares wall-clock values stamped by two hosts: the API
// writes enqueued_at when it admits the retry, the worker writes imported_at
// when the newer import finishes. Order of events for any retry this fence
// targets: the retry was admitted at T0 and held a lease; it could only be
// displaced after that lease lapsed, so the newer import was admitted no
// earlier than T0+faqImportLeaseTTL and completed at or after that. A worker
// clock lagging by up to one full lease TTL therefore still stamps the
// completion at or after T0 and the retry is caught; only a clock further off
// than that can push the stamp below the retry's own enqueue -- a deviation
// larger than any NTP-governed drift, and itself an incident. Forward skew can
// only refuse conservatively, which the user resolves by resubmitting: the
// fence's failure mode in that direction is a spurious refusal, never data
// loss.
func TestFAQStaleFenceClockSkewBound(t *testing.T) {
	retryAdmitted := time.Now().Truncate(time.Millisecond)
	payload := &types.FAQImportPayload{Mode: types.FAQBatchModeReplace,
		EnqueuedAt: retryAdmitted.Unix(), EnqueuedAtMillis: retryAdmitted.UnixMilli()}
	// The earliest the newer import could truly have completed: the retry's
	// lease lapsed one TTL after its admission and the newer import finished
	// immediately after being admitted. Any later completion only helps.
	earliestCompletion := retryAdmitted.Add(faqImportLeaseTTL)
	store := func(workerLag time.Duration) *types.FAQImportResult {
		at := earliestCompletion.Add(-workerLag) // a lagging clock stamps earlier
		return &types.FAQImportResult{TaskID: "prev", ImportedAt: at, ImportedAtMillis: at.UnixMilli()}
	}

	for _, tc := range []struct {
		name      string
		workerLag time.Duration
		wantStale bool
	}{
		{name: "in_sync", workerLag: 0, wantStale: true},
		{name: "lag_within_one_lease_TTL", workerLag: faqImportLeaseTTL - time.Minute, wantStale: true},
		{name: "lag_past_one_lease_TTL", workerLag: faqImportLeaseTTL + time.Minute, wantStale: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantStale, faqImportRetryIsStale(payload, store(tc.workerLag)))
		})
	}

	// Forward skew: a retry admitted AFTER the completion -- a healthy,
	// non-stale recovery -- is refused when the worker clock runs ahead past
	// its enqueue. Refusing here is safe (no deletes happen); the user is told
	// to resubmit.
	ahead := &types.FAQImportResult{TaskID: "prev", ImportedAt: retryAdmitted.Add(10 * time.Second)}
	ahead.ImportedAtMillis = ahead.ImportedAt.UnixMilli()
	require.True(t, faqImportRetryIsStale(payload, ahead),
		"forward skew can only ever refuse: the fence fails closed")
}
