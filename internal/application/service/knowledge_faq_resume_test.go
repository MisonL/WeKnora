package service

// A retry starts by writing a fresh progress record, and the batch loop saves
// the cursor only once a batch lands. An attempt that dies before its first
// batch must not wipe the cursor of the attempt before it: the next retry would
// then re-import from the start, replanning deletions over entries this task
// already wrote.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCarryFAQImportResumeCursorKeepsAValidCursor(t *testing.T) {
	existing := &types.FAQImportProgress{
		NextEntryIndex:    50,
		ValidEntryIndices: make([]int, 120),
		SuccessEntries:    []types.FAQSuccessEntry{{Index: 0}},
	}
	progress := &types.FAQImportProgress{NextEntryIndex: 0}
	carryFAQImportResumeCursor(progress, existing)
	require.Equal(t, 50, progress.NextEntryIndex, "the retry resumes where the last attempt stopped")
	require.Len(t, progress.SuccessEntries, 1, "entries imported before the failure stay reported")
}

func TestCarryFAQImportResumeCursorClampsACursorPastTheValidSet(t *testing.T) {
	// The cursor counts positions in validEntries; a record whose cursor is
	// past the end (a truncated or hand-edited record) must not index past it.
	existing := &types.FAQImportProgress{NextEntryIndex: 9, ValidEntryIndices: make([]int, 4)}
	progress := &types.FAQImportProgress{}
	carryFAQImportResumeCursor(progress, existing)
	require.Equal(t, 4, progress.NextEntryIndex)

	negative := &types.FAQImportProgress{NextEntryIndex: -3, ValidEntryIndices: make([]int, 4)}
	progress = &types.FAQImportProgress{}
	carryFAQImportResumeCursor(progress, negative)
	require.Zero(t, progress.NextEntryIndex)
}

func TestCarryFAQImportResumeCursorLeavesAFreshRecordAlone(t *testing.T) {
	progress := &types.FAQImportProgress{Message: "fresh"}
	carryFAQImportResumeCursor(progress, nil)
	require.Zero(t, progress.NextEntryIndex)
	require.Empty(t, progress.SuccessEntries)
	require.Equal(t, "fresh", progress.Message)
}

// A replace retry whose row deletions already happened must still hand those
// rows' vectors over in batches the queue accepts: the id list is whatever the
// knowledge base held, not the import's entry cap.
func TestFAQIndexDeleteFallbackSplitsThePayload(t *testing.T) {
	enqueuer := &recordingEnqueuer{}
	s := &knowledgeService{task: enqueuer}
	ctx := faqVectorFallbackContext()
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model"}

	ids := make([]string, 0, 450)
	for i := 0; i < 450; i++ {
		ids = append(ids, fmt.Sprintf("c%03d", i))
	}
	s.enqueueFAQIndexDeleteFallback(ctx, 1, kb, ids)

	require.Len(t, enqueuer.tasks, 3, "one task per batch, so no single message can be too large")
	var seen []string
	for _, task := range enqueuer.tasks {
		var payload types.IndexDeletePayload
		require.NoError(t, json.Unmarshal(task.Payload(), &payload))
		seen = append(seen, payload.ChunkIDs...)
	}
	require.ElementsMatch(t, ids, seen, "every id is handed over exactly once")
}

// No enqueuer (Lite without a registered handler) must not panic: the ids go to
// the log for an operator instead.
func TestFAQIndexDeleteFallbackWithoutAnEnqueuerIsANoOp(t *testing.T) {
	s := &knowledgeService{}
	s.enqueueFAQIndexDeleteFallback(faqVectorFallbackContext(), 1,
		&types.KnowledgeBase{ID: "kb"}, []string{"c1"})
}
