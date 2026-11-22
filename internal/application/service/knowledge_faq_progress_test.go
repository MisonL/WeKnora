package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// A completed dry run must report the plan it validated. Only a real import
// persists a result, so enriching a dry-run record from the knowledge would
// copy the previous real import's counts -- including its failure counts, which
// is exactly what a caller dry-runs to find out.
func TestCompletedDryRunKeepsItsOwnCounts(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })
	ctx := types.WithExecutionTenant(f.ctx, 7)

	knowledge, err := f.svc.repo.GetKnowledgeByID(ctx, 7, "faq")
	require.NoError(t, err)
	require.NoError(t, knowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TotalEntries: 80, SuccessCount: 50, FailedCount: 9, PartialFailedCount: 4,
		SkippedCount: 2, ImportMode: "replace",
	}))
	require.NoError(t, f.svc.repo.UpdateKnowledge(ctx, knowledge))

	require.NoError(t, f.svc.saveFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: "dry", KBID: "kb", KnowledgeID: "faq",
		Status: types.FAQImportStatusCompleted, Progress: 100,
		Total: 10, Processed: 10,
		SuccessCount: 7, FailedCount: 2, PartialFailedCount: 1,
		DryRun: true, CreatedAt: 1,
	}))

	dry, err := f.svc.GetFAQImportProgress(ctx, "dry")
	require.NoError(t, err)
	require.Equal(t, 7, dry.SuccessCount)
	require.Equal(t, 2, dry.FailedCount)
	require.Equal(t, 1, dry.PartialFailedCount)
	require.Zero(t, dry.SkippedCount)
	// The previous real import's result fields stay out of a dry run's record.
	require.Empty(t, dry.ImportMode)
	require.True(t, dry.DryRun)
}

// The guard is specific to dry runs: a completed real import's record is still
// enriched from the persisted result, which is what carries its display fields.
//
// Only the record of the import that wrote that result may be enriched. An
// older task's record still reads "completed" in Redis for its whole TTL (three
// hours), so enriching it too would answer a query about last week's task with
// this afternoon's numbers.
func TestCompletedImportIsEnrichedFromThePersistedResult(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })
	ctx := types.WithExecutionTenant(f.ctx, 7)

	knowledge, err := f.svc.repo.GetKnowledgeByID(ctx, 7, "faq")
	require.NoError(t, err)
	require.NoError(t, knowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID: "real", TotalEntries: 80, SuccessCount: 50, FailedCount: 9, PartialFailedCount: 4,
		SkippedCount: 2, ImportMode: "replace", DisplayStatus: "open",
	}))
	require.NoError(t, f.svc.repo.UpdateKnowledge(ctx, knowledge))

	require.NoError(t, f.svc.saveFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: "real", KBID: "kb", KnowledgeID: "faq",
		Status: types.FAQImportStatusCompleted, Total: 80, Processed: 80,
		CreatedAt: 1,
	}))

	real, err := f.svc.GetFAQImportProgress(ctx, "real")
	require.NoError(t, err)
	require.Equal(t, 50, real.SuccessCount)
	require.Equal(t, 9, real.FailedCount)
	require.Equal(t, 4, real.PartialFailedCount)
	require.Equal(t, 2, real.SkippedCount)
	require.Equal(t, "replace", real.ImportMode)
	require.Equal(t, "open", real.DisplayStatus)
}

// A completed record for some other task keeps its own numbers: the persisted
// result belongs to the task it names, not to every query that happens to be
// completed.
func TestCompletedRecordOfAnotherTaskKeepsItsOwnCounts(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })
	ctx := types.WithExecutionTenant(f.ctx, 7)

	knowledge, err := f.svc.repo.GetKnowledgeByID(ctx, 7, "faq")
	require.NoError(t, err)
	require.NoError(t, knowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID: "newer", TotalEntries: 80, SuccessCount: 50, FailedCount: 9, PartialFailedCount: 4,
		SkippedCount: 2, ImportMode: "replace", DisplayStatus: "open",
	}))
	require.NoError(t, f.svc.repo.UpdateKnowledge(ctx, knowledge))

	require.NoError(t, f.svc.saveFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: "older", KBID: "kb", KnowledgeID: "faq",
		Status: types.FAQImportStatusCompleted, Total: 12, Processed: 12,
		SuccessCount: 12, CreatedAt: 1,
	}))

	older, err := f.svc.GetFAQImportProgress(ctx, "older")
	require.NoError(t, err)
	require.Equal(t, 12, older.SuccessCount)
	require.Zero(t, older.FailedCount)
	require.Zero(t, older.PartialFailedCount)
	require.Empty(t, older.ImportMode)
}

// A shared knowledge base is polled by members of other tenants. The completion
// enrichment has to read the knowledge row under the tenant that ran the
// import -- the record's own tenant -- or those callers get the bare Redis
// record without the persisted counts and display fields.
func TestCompletedRecordIsEnrichedForAPollingMemberOfAnotherTenant(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })
	ownerCtx := types.WithExecutionTenant(f.ctx, 7)

	knowledge, err := f.svc.repo.GetKnowledgeByID(ownerCtx, 7, "faq")
	require.NoError(t, err)
	require.NoError(t, knowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID: "shared", TotalEntries: 80, SuccessCount: 50, FailedCount: 9,
		PartialFailedCount: 4, SkippedCount: 2, ImportMode: "replace", DisplayStatus: "open",
	}))
	require.NoError(t, f.svc.repo.UpdateKnowledge(ownerCtx, knowledge))

	require.NoError(t, f.svc.saveFAQImportProgress(ownerCtx, &types.FAQImportProgress{
		TaskID: "shared", KBID: "kb", KnowledgeID: "faq", TenantID: 7,
		Status: types.FAQImportStatusCompleted, Total: 80, Processed: 80,
		CreatedAt: 1,
	}))

	memberCtx := types.WithExecutionTenant(f.ctx, 99)
	got, err := f.svc.GetFAQImportProgress(memberCtx, "shared")
	require.NoError(t, err)
	require.Equal(t, 50, got.SuccessCount, "the owner tenant's persisted result must be applied")
	require.Equal(t, "open", got.DisplayStatus)
}
