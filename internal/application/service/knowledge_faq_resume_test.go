package service

// A retry starts by writing a fresh progress record, and the batch loop saves
// the cursor only once a batch lands. An attempt that dies before its first
// batch must not wipe the cursor of the attempt before it: the next retry would
// then re-import from the start, replanning deletions over entries this task
// already wrote.

import (
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
