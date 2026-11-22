package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// deletingKBKnowledgeRepo mirrors what the delete task sees: the rows are
// returned by its first listing and are gone (soft-deleted) by the time the
// cleanup passes list them again. That is why the release cannot rely on a
// second list alone.
type deletingKBKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	mu    sync.Mutex
	items []*types.Knowledge
	calls int
}

func (r *deletingKBKnowledgeRepo) ListKnowledgeByKnowledgeBaseID(
	context.Context, uint64, string,
) ([]*types.Knowledge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls > 1 {
		return nil, nil
	}
	return r.items, nil
}

func (r *deletingKBKnowledgeRepo) DeleteKnowledgeList(context.Context, uint64, []string) error {
	return nil
}

// Every replica runs workers, so the queue cancel of a deleted KB is not
// enough: a queued parse job that never runs keeps the budget its uploader
// charged for the life of the process. The async delete task is the durable
// backstop and has to free the rows it listed, even though the second listing
// inside the cleanup pass no longer sees them.
func TestProcessKBDeleteReleasesParseWorkForItsKnowledgeRows(t *testing.T) {
	withUploadBackgroundBudget(t, 100)
	const kbID = "kb-delete"
	repo := &deletingKBKnowledgeRepo{
		items: []*types.Knowledge{{ID: "k-queued", KnowledgeBaseID: kbID}},
	}
	svc := &knowledgeBaseService{
		kgRepo:        repo,
		chunkRepo:     kbCleanupChunkRepo{},
		modelService:  kbCleanupModelService{},
		taskInspector: &recordingKBTaskInspector{},
		wikiRepo:      &recordingWikiRepo{},
	}

	release, err := ReserveUploadWork("k-queued", 70, requestUploadWork)
	require.NoError(t, err)
	defer release()
	require.Equal(t, int64(70), uploadWorkHeld())

	require.NoError(t, svc.ProcessKBDelete(context.Background(), kbDeletePayload(t, kbID, 1)))

	require.Equal(t, int64(0), uploadWorkHeld(), "a queued parse job of a deleted row never releases on its own")
}
