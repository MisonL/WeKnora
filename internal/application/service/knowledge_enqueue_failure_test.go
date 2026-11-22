package service

import (
	"context"
	"errors"
	"testing"

	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

// enqueueFailingTaskEnqueuer refuses every enqueue, standing in for a Redis
// outage or a queue that cannot accept the task.
type enqueueFailingTaskEnqueuer struct {
	err   error
	calls int
}

func (e *enqueueFailingTaskEnqueuer) Enqueue(*asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error) {
	e.calls++
	return nil, e.err
}

// enqueueFailingRepo records the final state written back when the enqueue
// fails, so the test can assert the row is left marked failed rather than
// pending forever.
type enqueueFailingRepo struct {
	interfaces.KnowledgeRepository

	created  *types.Knowledge
	updated  *types.Knowledge
	updateNo int
}

func (r *enqueueFailingRepo) CheckKnowledgeExists(
	context.Context, uint64, string, *types.KnowledgeCheckParams,
) (bool, *types.Knowledge, error) {
	return false, nil, nil
}

func (r *enqueueFailingRepo) CreateKnowledge(_ context.Context, knowledge *types.Knowledge) error {
	copied := *knowledge
	r.created = &copied
	return nil
}

func (r *enqueueFailingRepo) UpdateKnowledge(_ context.Context, knowledge *types.Knowledge) error {
	r.updateNo++
	copied := *knowledge
	r.updated = &copied
	return nil
}

func (r *enqueueFailingRepo) GetKnowledgeTags(
	context.Context, []string,
) (map[string][]*types.KnowledgeTag, error) {
	return map[string][]*types.KnowledgeTag{}, nil
}

// TestCreateKnowledgeFromFileReportsAFailedEnqueue is the contract the upload
// endpoint rests on: a row whose parse job never reached the queue must not be
// reported as an accepted upload.
//
// Before this, the service returned (row, nil) and the handler answered 200.
// A caller had no way to tell a document that is being parsed from one that
// will sit in "pending" forever, and the CLI printed a checkmark for it.
func TestCreateKnowledgeFromFileReportsAFailedEnqueue(t *testing.T) {
	t.Parallel()

	queueErr := errors.New("redis unavailable")
	repo := &enqueueFailingRepo{}
	enqueuer := &enqueueFailingTaskEnqueuer{err: queueErr}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   &createKnowledgeFileServiceStub{},
		task:      enqueuer,
	}

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.Error(t, err, "an unqueued document must not be reported as accepted")
	require.Equal(t, 1, enqueuer.calls)

	// The row is still handed back: the file is stored and a reparse can pick
	// it up without re-uploading.
	require.NotNil(t, knowledge, "the stored row must accompany the error so it can be reparsed")
	require.NotEmpty(t, knowledge.ID)

	appErr, ok := werrors.IsAppError(err)
	require.True(t, ok, "the failure must be a typed application error, got %T", err)
	require.Equal(t, werrors.ErrKnowledgeEnqueueFailed, appErr.Code)
	require.Equal(t, 503, appErr.HTTPCode,
		"a stored-but-unqueued document is a temporary server failure, not a bad request")

	// The row must be persisted as failed, so the document list shows the
	// truth and the client's poll does not wait on a job that never existed.
	require.NotNil(t, repo.created)
	require.Equal(t, 1, repo.updateNo, "the failed status must be written back exactly once")
	require.NotNil(t, repo.updated)
	require.Equal(t, types.ParseStatusFailed, repo.updated.ParseStatus)
}

// TestEnqueueFailureCarriesTheRowIdentity pins the details payload: an agent
// reading the 503 has to be able to name the row it should reparse.
func TestEnqueueFailureCarriesTheRowIdentity(t *testing.T) {
	t.Parallel()

	knowledge := &types.Knowledge{ID: "kn-9", Title: "手册.pdf", ParseStatus: types.ParseStatusFailed}
	err := enqueueFailedError(knowledge)

	appErr, ok := werrors.IsAppError(err)
	require.True(t, ok)
	details, ok := appErr.Details.(map[string]any)
	require.True(t, ok, "details must be structured, got %T", appErr.Details)
	require.Equal(t, "kn-9", details["knowledge_id"])
	require.Equal(t, "手册.pdf", details["title"])
	require.Equal(t, types.ParseStatusFailed, details["parse_status"])
}

// TestParseBudgetRefusalKeepsTheRetryHint pins the 429 the upload returns when
// the document was stored but the parse-work budget is full.
//
// The message is re-worded for this call site (the file is safe, only the job
// is missing), but the hint and the row identity are the admission's and a
// client needs both: without the hint it waits the wrong amount, and without
// the row it treats "stored, reparse later" as a plain admission refusal and
// uploads the same bytes again -- which the duplicate check does not stop,
// because failed rows are excluded from it.
func TestParseBudgetRefusalKeepsTheRetryHint(t *testing.T) {
	t.Setenv("UPLOAD_MAX_BACKGROUND_BYTES", "1")
	resetUploadWork()
	t.Cleanup(resetUploadWork)

	repo := &enqueueFailingRepo{}
	enqueuer := &enqueueFailingTaskEnqueuer{}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   &createKnowledgeFileServiceStub{},
		task:      enqueuer,
	}

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.Error(t, err)
	require.NotNil(t, knowledge, "the stored row must accompany the refusal")
	require.Equal(t, 0, enqueuer.calls, "no job may be queued once the budget refused it")

	appErr, ok := werrors.IsAppError(err)
	require.True(t, ok, "the refusal must be a typed application error, got %T", err)
	require.Equal(t, werrors.ErrTooManyRequests, appErr.Code)
	require.Equal(t, 429, appErr.HTTPCode)
	require.Positive(t, appErr.RetryAfterSeconds,
		"the budget's own Retry-After must survive the call site re-wording")

	details, ok := appErr.Details.(map[string]any)
	require.True(t, ok, "details must be structured, got %T", appErr.Details)
	require.Equal(t, knowledge.ID, details["knowledge_id"],
		"the row identity is what separates this refusal from one that stored nothing")
}
