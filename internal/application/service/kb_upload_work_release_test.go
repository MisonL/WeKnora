package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// kbRowsOnlyRepo serves just the listing ListKnowledgeByKnowledgeBaseID needs;
// every other repository method is nil (unused by the release helper).
type kbRowsOnlyRepo struct {
	interfaces.KnowledgeRepository
	rows []*types.Knowledge
	err  error
}

func (r *kbRowsOnlyRepo) ListKnowledgeByKnowledgeBaseID(
	context.Context, uint64, string,
) ([]*types.Knowledge, error) {
	return r.rows, r.err
}

// TestDeletingAKnowledgeBaseReleasesItsQueuedParseBudget pins the whole-KB
// counterpart of the per-row cancel path: rows still queued when the KB is
// deleted never reach a worker, so the bytes their uploads reserved must be
// given back here or they stay charged until the process restarts.
func TestDeletingAKnowledgeBaseReleasesItsQueuedParseBudget(t *testing.T) {
	resetUploadWork()
	t.Cleanup(resetUploadWork)

	release, err := ReserveUploadWork("kn-a", 4096, requestUploadWork)
	require.NoError(t, err)
	defer release() // the row is never dequeued, so this is the only release
	require.Equal(t, int64(4096), uploadWorkHeld())

	svc := &knowledgeBaseService{kgRepo: &kbRowsOnlyRepo{rows: []*types.Knowledge{{ID: "kn-a"}}}}
	svc.onKnowledgeBaseDeleted(context.Background(), 1, "kb-1")

	require.Equal(t, int64(0), uploadWorkHeld(),
		"a deleted KB's queued parse reservations must be released")
}

// A listing failure must not panic and must not release anything.
func TestReleaseUploadWorkForKnowledgeBaseToleratesListError(t *testing.T) {
	resetUploadWork()
	t.Cleanup(resetUploadWork)

	release, err := ReserveUploadWork("kn-b", 128, requestUploadWork)
	require.NoError(t, err)
	defer release()

	svc := &knowledgeBaseService{kgRepo: &kbRowsOnlyRepo{err: context.DeadlineExceeded}}
	svc.releaseUploadWorkForKnowledgeBase(context.Background(), 1, "kb-1")

	require.Equal(t, int64(128), uploadWorkHeld())
}
