package service

// Third review round: three follow-ups that the second round either left open
// or introduced.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// A replay of a completed record used to be checked only after the running
// marker was consulted, so a replay arriving while another import held the
// knowledge base was first rewritten to "failed" by that check and the
// completed result the initiator polls disappeared.
func TestCompletedFAQImportReplayLeavesTheRecordAloneWhileAnotherImportRuns(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })

	payload := types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq",
		TaskID: "task-a", InstanceID: "inst-a", Mode: types.FAQBatchModeReplace,
		Entries: []types.FAQEntryPayload{{
			StandardQuestion: "How do I reset my password?",
			Answers:          []string{"Use the reset link."},
		}},
	}
	ctx := types.WithExecutionTenant(context.Background(), payload.TenantID)
	completed := &types.FAQImportProgress{
		TaskID: "task-a", KBID: "kb", KnowledgeID: "faq", TenantID: 7,
		InstanceID: "inst-a", Status: types.FAQImportStatusCompleted,
		Total: 1, SuccessCount: 1, ValidEntryIndices: []int{0}, Message: "done",
	}
	require.NoError(t, f.svc.saveFAQImportProgress(ctx, completed))
	require.NoError(t, f.svc.setRunningFAQImportInfo(ctx, "kb",
		&runningFAQImportInfo{TaskID: "task-b", InstanceID: "inst-b"}))

	err := f.svc.ProcessFAQImport(ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload)))
	require.NoError(t, err, "a replay of a completed record is acknowledged without rewriting it")

	stored, getErr := f.svc.GetFAQImportProgress(ctx, "task-a")
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	require.Equal(t, types.FAQImportStatusCompleted, stored.Status,
		"the running-marker check of another import must not rewrite this record to failed")
}

// Validation removes conflicting similar and negative questions in place and
// persists only the index list. Reusing that list on a retry that re-decodes
// the original payload therefore imports entries the first pass had cleaned.
func TestFAQValidationIsNotReusedAfterTheFirstPassSanitizedEntries(t *testing.T) {
	payload := &types.FAQImportPayload{TaskID: "task", InstanceID: "inst-a", Entries: make([]types.FAQEntryPayload, 2)}

	require.False(t,
		faqValidationReusable(&types.FAQImportProgress{
			InstanceID: "inst-a", ValidEntryIndices: []int{0, 1}, PartialFailedCount: 1,
		}, payload),
		"an attempt whose validation edited entries must re-validate, so the edits are made again")

	require.True(t,
		faqValidationReusable(&types.FAQImportProgress{
			InstanceID: "inst-a", ValidEntryIndices: []int{0, 1},
		}, payload),
		"a pass that only dropped whole entries left the kept entries untouched")

	dryRun := &types.FAQImportProgress{InstanceID: "inst-a", ValidEntryIndices: []int{0, 1}, DryRun: true}
	require.False(t, faqValidationReusable(dryRun, payload),
		"a dry run's validation result answers a different question")

	require.False(t,
		faqValidationReusable(&types.FAQImportProgress{
			InstanceID: "inst-a", ValidEntryIndices: []int{0, 5},
		}, payload),
		"an index past the payload cannot be reused")
	require.False(t, faqValidationReusable(nil, payload))
}

// A record written by a later submission that reused this task id describes a
// different payload. Treating it as this run's own completed result would
// acknowledge a task whose entries were never imported. The same holds for a
// cached validation result: only the run that produced it may reuse it.
func TestFAQProgressOwnershipFollowsTheInstanceID(t *testing.T) {
	payload := &types.FAQImportPayload{TaskID: "task", InstanceID: "inst-a"}

	require.True(t, faqProgressOwnedByInstance(&types.FAQImportProgress{InstanceID: "inst-a"}, payload))
	require.False(t, faqProgressOwnedByInstance(&types.FAQImportProgress{InstanceID: "inst-b"}, payload),
		"another instance's record is not this run's result")
	// A record and a payload that each lack an instance id are both legacy
	// enqueues and still match on the task id alone.
	require.True(t, faqProgressOwnedByInstance(&types.FAQImportProgress{}, &types.FAQImportPayload{TaskID: "task"}),
		"two legacy records are the same run")
	// A mixed pair has no identity to compare: a legacy payload must not claim
	// a modern record (its validation indices address a different payload), and
	// neither may a modern payload claim a legacy record.
	require.False(t, faqProgressOwnedByInstance(
		&types.FAQImportProgress{InstanceID: "inst-a"}, &types.FAQImportPayload{TaskID: "task"}),
		"a legacy payload does not own a modern record")
	require.False(t, faqProgressOwnedByInstance(
		&types.FAQImportProgress{}, &types.FAQImportPayload{TaskID: "task", InstanceID: "inst-a"}),
		"a modern payload does not own a legacy record")
}

// partialDeleteChunkRepo deletes part of the plan and then fails, the way the
// batched conditional delete does when a later batch's statement fails.
type partialDeleteChunkRepo struct {
	interfaces.ChunkRepository
	deleted []string
	err     error
}

func (r *partialDeleteChunkRepo) UnchangedChunks(
	_ context.Context, _ uint64, chunks []*types.Chunk,
) ([]*types.Chunk, error) {
	return chunks, nil
}

func (r *partialDeleteChunkRepo) DeleteChunksIfUnchanged(
	_ context.Context, _ uint64, _ []*types.Chunk,
) ([]string, error) {
	return r.deleted, r.err
}

// The conditional delete runs in batches; when a later batch fails, the rows
// the earlier batches already soft-deleted are gone from every later plan. The
// ids it returns on the error path are the only record of them, so their
// vectors must still be handed to the index-delete task.
func TestDeleteReplacedFAQChunksHandsOverRowsDeletedBeforeAFailedBatch(t *testing.T) {
	enqueuer := &recordingEnqueuer{}
	s := &knowledgeService{
		chunkRepo: &partialDeleteChunkRepo{
			deleted: []string{"c1", "c2"},
			err:     errors.New("connection reset"),
		},
		modelService:   faqVectorEmbeddings{embedding: &faqVectorEmbedder{}},
		retrieveEngine: faqVectorRegistry{engine: &faqVectorEngine{err: errors.New("vector store unavailable")}},
		task:           enqueuer,
	}
	ctx := faqVectorFallbackContext()
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model"}
	planned := []*types.Chunk{
		{ID: "c1", TenantID: 1, KnowledgeBaseID: "kb"},
		{ID: "c2", TenantID: 1, KnowledgeBaseID: "kb"},
		{ID: "c3", TenantID: 1, KnowledgeBaseID: "kb"},
	}

	err := s.deleteReplacedFAQChunks(ctx, "task-1", 1, kb, &types.Knowledge{ID: "knowledge"}, planned)
	require.Error(t, err, "the import still reports the failed delete")

	// The first task covers the whole plan and is queued before the rows go.
	// The failed batch sequence then reports the ids it already committed, and
	// the vector backend failure hands the same ids over once more; every task
	// is idempotent.
	require.Len(t, enqueuer.tasks, 3,
		"the rows already deleted by an earlier batch have no other recovery path")
	var taskIDs [][]string
	for _, task := range enqueuer.tasks {
		require.Equal(t, types.TypeIndexDelete, task.Type())
		var payload types.IndexDeletePayload
		require.NoError(t, json.Unmarshal(task.Payload(), &payload))
		require.True(t, payload.OnlyIfDeleted, "only rows that are actually soft-deleted may lose their vectors")
		taskIDs = append(taskIDs, payload.ChunkIDs)
	}
	require.ElementsMatch(t, []string{"c1", "c2", "c3"}, taskIDs[0],
		"the pre-delete task covers the whole plan")
	for _, ids := range taskIDs[1:] {
		require.ElementsMatch(t, []string{"c1", "c2"}, ids,
			"only rows this call actually deleted; c3 is still live and keeps its vectors")
	}
}
