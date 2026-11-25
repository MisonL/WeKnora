package service

// Fourth review round: replace-mode safety, lease-time staleness, terminal
// progress records, fenced deletes, index-delete crash coverage, result
// finalization, instance ownership and tenant-scoped progress keys.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// recordingChunkCreator stands in for ChunkService: it records the import's
// creates and delegates row persistence to the fixture's repository.
type recordingChunkCreator struct {
	interfaces.ChunkService
	inner   interfaces.ChunkService
	repo    interfaces.ChunkRepository
	created []*types.Chunk
}

func (c *recordingChunkCreator) CreateChunks(ctx context.Context, chunks []*types.Chunk) error {
	c.created = append(c.created, chunks...)
	return c.repo.CreateChunks(ctx, chunks)
}

func (c *recordingChunkCreator) DeleteChunk(ctx context.Context, id string) error {
	if c.inner == nil {
		return nil
	}
	return c.inner.DeleteChunk(ctx, id)
}

// recordingFileService is a small in-memory object store that records deletes.
type recordingFileService struct {
	interfaces.FileService
	objects map[string][]byte
	deleted []string
	events  *[]string
	getErr  error
	saveErr error
}

func (f *recordingFileService) SaveBytes(
	_ context.Context, data []byte, _ uint64, fileName string, _ bool,
) (string, error) {
	if f.saveErr != nil {
		return "", f.saveErr
	}
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	path := "mem/" + fileName
	f.objects[path] = append([]byte(nil), data...)
	return path, nil
}

func (f *recordingFileService) GetFile(_ context.Context, path string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[path]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *recordingFileService) DeleteFile(_ context.Context, path string) error {
	f.deleted = append(f.deleted, path)
	if f.events != nil {
		*f.events = append(*f.events, "file-delete")
	}
	return nil
}

// faqKnowledgeRepoSpy fails one selected read (the lease-time re-read) or every
// result write.
type faqKnowledgeRepoSpy struct {
	interfaces.KnowledgeRepository
	mu        sync.Mutex
	getCalls  int
	failGetAt int
	getErr    error
	updateErr error
	events    *[]string
}

func (r *faqKnowledgeRepoSpy) GetKnowledgeByID(ctx context.Context, tenantID uint64, id string) (*types.Knowledge, error) {
	r.mu.Lock()
	r.getCalls++
	fail := r.failGetAt > 0 && r.getCalls == r.failGetAt
	r.mu.Unlock()
	if fail {
		return nil, r.getErr
	}
	return r.KnowledgeRepository.GetKnowledgeByID(ctx, tenantID, id)
}

func (r *faqKnowledgeRepoSpy) UpdateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	if r.events != nil {
		*r.events = append(*r.events, "db-update")
	}
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.KnowledgeRepository.UpdateKnowledge(ctx, knowledge)
}

func faqStrategy(s types.AnswerStrategy) *types.AnswerStrategy { return &s }

// Defect: an unknown answer_strategy passed the basic validation, the replace
// planner's sanitiser then skipped it ("skipped: 1, to create: 0"), and the
// deletion set still removed every old row. The import reported success.
func TestReplaceImportRejectsUnknownAnswerStrategyBeforeDeleting(t *testing.T) {
	f := newFAQWriteFixture(t)

	payload := types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-strategy",
		InstanceID: "inst-strategy", Mode: types.FAQBatchModeReplace,
		Entries: []types.FAQEntryPayload{{
			StandardQuestion: "How do I reset my password?",
			Answers:          []string{"Use the reset link."},
			AnswerStrategy:   faqStrategy(types.AnswerStrategy("ALL")),
		}},
	}
	pending := &types.FAQImportProgress{
		TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
		TenantID: 7, InstanceID: payload.InstanceID, Status: types.FAQImportStatusPending, Total: 1,
	}
	require.NoError(t, f.svc.saveFAQImportProgress(f.ctx, pending))

	err := f.svc.ProcessFAQImport(f.ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload)))
	require.NoError(t, err, "an invalid entry is a recorded failure, not an import error")

	// The old entry is still there: nothing was planned for creation, so
	// nothing may be deleted.
	_, getErr := f.chunks.GetChunkByID(f.ctx, 7, "one")
	require.NoError(t, getErr, "the replaced entry's chunk must survive a batch that imports nothing")
	require.Empty(t, f.creator.created)

	progress, err := f.svc.GetFAQImportProgress(f.ctx, payload.TaskID)
	require.NoError(t, err)
	require.Equal(t, 1, progress.FailedCount)
	require.NotEmpty(t, progress.FailedEntriesURL, "the failure detail is exported for the caller")
	require.Contains(t, string(f.files.objects[progress.FailedEntriesURL]), "answer_strategy")
}

// Defect: validation accepted an entry and the planner silently skipped it,
// which let the replace deletion run with nothing to create. Any planning-time
// rejection is an inconsistency and must stop the import.
func TestReplacePlanningStopsOnAnEntryValidationAccepted(t *testing.T) {
	f := newFAQWriteFixture(t)

	t.Run("invalid strategy reaching the planner", func(t *testing.T) {
		_, _, _, _, err := f.svc.calculateReplaceOperations(f.ctx, 7, "faq", []types.FAQEntryPayload{{
			StandardQuestion: "question one",
			Answers:          []string{"answer"},
			AnswerStrategy:   faqStrategy(types.AnswerStrategy("ALL")),
		}})
		require.Error(t, err, "the planner must not skip an entry after validation counted it valid")
	})

	t.Run("duplicate standard question reaching the planner", func(t *testing.T) {
		_, _, _, _, err := f.svc.calculateReplaceOperations(f.ctx, 7, "faq", []types.FAQEntryPayload{
			{StandardQuestion: "question one", Answers: []string{"answer"}},
			{StandardQuestion: "question one", Answers: []string{"another answer"}},
		})
		require.Error(t, err, "a duplicate the validator should have removed must stop the import")
	})
}

// Defect: a failed lease-time staleness re-read only logged a warning and the
// attempt continued, so a stale replace retry could delete what a newer import
// added.
func TestLeaseTimeReReadFailureStopsTheAttempt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantSkip    bool
		wantFailed  bool
		description string
	}{
		{
			name: "row no longer exists", err: repository.ErrKnowledgeNotFound, wantSkip: true, wantFailed: true,
			description: "a deleted document ends the task and closes its record",
		},
		{
			name: "transient read failure", err: errors.New("database temporarily unavailable"),
			wantSkip: false, wantFailed: false,
			description: "a transient failure is retried, not treated as an empty result",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFAQWriteFixture(t)
			payload := types.FAQImportPayload{
				TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-fresh",
				InstanceID: "inst-fresh", Mode: types.FAQBatchModeReplace,
				Entries: []types.FAQEntryPayload{{
					StandardQuestion: "How do I reset my password?",
					Answers:          []string{"Use the reset link."},
				}},
			}
			pending := &types.FAQImportProgress{
				TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
				TenantID: 7, InstanceID: payload.InstanceID, Status: types.FAQImportStatusPending, Total: 1,
			}
			require.NoError(t, f.svc.saveFAQImportProgress(f.ctx, pending))

			// The first read is the pre-lease staleness check; the second is
			// the lease-time re-read under test.
			f.svc.repo = &faqKnowledgeRepoSpy{KnowledgeRepository: f.svc.repo, failGetAt: 2, getErr: tc.err}

			err := f.svc.ProcessFAQImport(f.ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload)))
			require.Error(t, err, tc.description)
			if tc.wantSkip {
				require.ErrorIs(t, err, asynq.SkipRetry)
			} else {
				require.NotErrorIs(t, err, asynq.SkipRetry)
			}
			require.Empty(t, f.creator.created, "nothing may be written after the re-read failed")

			progress, getErr := f.svc.GetFAQImportProgress(f.ctx, payload.TaskID)
			require.NoError(t, getErr)
			if tc.wantFailed {
				require.Equal(t, types.FAQImportStatusFailed, progress.Status)
				require.Contains(t, progress.Error, "不存在")
			} else {
				require.Equal(t, types.FAQImportStatusPending, progress.Status)
			}
		})
	}
}

// Defect: markFAQImportStopped rewrote a completed record to "failed" when a
// late replay or takeover was noticed, erasing the result the initiator saw.
func TestMarkFAQImportStoppedKeepsACompletedRecord(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = f.svc.redisClient.Close() })

	completed := &types.FAQImportProgress{
		TaskID: "task-a", KBID: "kb", KnowledgeID: "faq", TenantID: 7, InstanceID: "inst-a",
		Status: types.FAQImportStatusCompleted, Progress: 100, Total: 1, SuccessCount: 1, CreatedAt: 1,
	}
	require.NoError(t, f.svc.writeFAQImportProgress(f.ctx, completed))

	f.svc.markFAQImportStopped(f.ctx, "task-a", "inst-a", "stopped")

	stored, err := f.svc.GetFAQImportProgress(f.ctx, "task-a")
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusCompleted, stored.Status,
		"a finished import is never downgraded to failed")
	require.Empty(t, stored.Error)
	require.Equal(t, 1, stored.SuccessCount)
}

// The end-to-end shape of the same defect: import A completed, import B then
// completed, and A's replace task is replayed. The stale-result refusal records
// A as stopped, which used to rewrite A's completed record to "failed" and hide
// the result its initiator already saw.
func TestStaleReplaceReplayKeepsTheCompletedRecord(t *testing.T) {
	f := newFAQWriteFixture(t)

	knowledge, err := f.svc.repo.GetKnowledgeByID(f.ctx, 7, "faq")
	require.NoError(t, err)
	require.NoError(t, knowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID: "task-b", InstanceID: "inst-b", ImportMode: types.FAQBatchModeReplace,
		ImportedAt: time.Now(), ImportedAtMillis: time.Now().UnixMilli(),
	}))
	require.NoError(t, f.svc.repo.UpdateKnowledge(f.ctx, knowledge))

	payload := types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-a",
		InstanceID: "inst-a", Mode: types.FAQBatchModeReplace,
		EnqueuedAt: time.Now().Add(-time.Hour).Unix(), EnqueuedAtMillis: time.Now().Add(-time.Hour).UnixMilli(),
		Entries: []types.FAQEntryPayload{{
			StandardQuestion: "How do I reset my password?",
			Answers:          []string{"Use the reset link."},
		}},
	}
	require.NoError(t, f.svc.writeFAQImportProgress(f.ctx, &types.FAQImportProgress{
		TaskID: "task-a", KBID: "kb", KnowledgeID: "faq", TenantID: 7, InstanceID: "inst-a",
		Status: types.FAQImportStatusCompleted, Progress: 100, Total: 1, SuccessCount: 1, CreatedAt: 1,
	}))

	require.ErrorIs(t, f.svc.ProcessFAQImport(f.ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload))),
		asynq.SkipRetry, "the stale replay is refused")

	stored, err := f.svc.GetFAQImportProgress(f.ctx, "task-a")
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusCompleted, stored.Status,
		"the completed record of task A must survive task B's later result")
	require.Equal(t, 1, stored.SuccessCount)
	require.Empty(t, f.creator.created)
}

// recordingVectorEngine records DeleteByChunkIDList batches.
type recordingVectorEngine struct {
	interfaces.RetrieveEngineService
	mu      sync.Mutex
	batches [][]string
	err     error
}

func (*recordingVectorEngine) EngineType() types.RetrieverEngineType {
	return types.SQLiteRetrieverEngineType
}

func (*recordingVectorEngine) Support() []types.RetrieverType {
	return []types.RetrieverType{types.VectorRetrieverType}
}

func (*recordingVectorEngine) EstimateStorageSize(
	context.Context, embedding.Embedder, []*types.IndexInfo, []types.RetrieverType,
) int64 {
	return 0
}

func (e *recordingVectorEngine) DeleteByChunkIDList(_ context.Context, ids []string, _ int, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.batches = append(e.batches, append([]string(nil), ids...))
	return nil
}

type recordingVectorRegistry struct {
	interfaces.RetrieveEngineRegistry
	engine interfaces.RetrieveEngineService
}

func (r recordingVectorRegistry) GetRetrieveEngineService(
	types.RetrieverEngineType,
) (interfaces.RetrieveEngineService, error) {
	return r.engine, nil
}

func faqVectorEngineParams() []types.RetrieverEngineParams {
	return []types.RetrieverEngineParams{{
		RetrieverEngineType: types.SQLiteRetrieverEngineType,
		RetrieverType:       types.VectorRetrieverType,
	}}
}

func newFAQVectorChunkRepo(t *testing.T) interfaces.ChunkRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	return repository.NewChunkRepository(db)
}

// Defect: an index-delete enqueued before a row delete could remove the
// vectors of rows the delete never reached. OnlyIfDeleted restricts the worker
// to rows whose chunk row is already soft-deleted.
func TestProcessIndexDeleteOnlyTouchesDeletedRowsWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name          string
		onlyIfDeleted bool
		want          []string
	}{
		{name: "restricted to deleted rows", onlyIfDeleted: true, want: []string{"dead"}},
		{name: "unrestricted deletes every listed id", onlyIfDeleted: false, want: []string{"live", "dead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFAQVectorChunkRepo(t)
			ctx := context.Background()
			require.NoError(t, repo.CreateChunks(ctx, []*types.Chunk{
				{ID: "live", TenantID: 7, KnowledgeBaseID: "kb", KnowledgeID: "k", ChunkType: types.ChunkTypeFAQ, Content: "q"},
				{ID: "dead", TenantID: 7, KnowledgeBaseID: "kb", KnowledgeID: "k", ChunkType: types.ChunkTypeFAQ, Content: "q"},
			}))
			require.NoError(t, repo.DeleteChunk(ctx, 7, "dead"))

			engine := &recordingVectorEngine{}
			svc := &knowledgeTagService{
				chunkRepo:      repo,
				retrieveEngine: recordingVectorRegistry{engine: engine},
				modelService:   faqVectorEmbeddings{embedding: &faqVectorEmbedder{}},
			}
			payloadBytes := mustWriteJSON(t, types.IndexDeletePayload{
				TenantID: 7, KnowledgeBaseID: "kb", EmbeddingModelID: "model",
				KBType: types.KnowledgeTypeFAQ, ChunkIDs: []string{"live", "dead"},
				EffectiveEngines: faqVectorEngineParams(), OnlyIfDeleted: tc.onlyIfDeleted,
			})
			require.NoError(t, svc.ProcessIndexDelete(ctx, asynq.NewTask(types.TypeIndexDelete, payloadBytes)))

			var seen []string
			for _, batch := range engine.batches {
				seen = append(seen, batch...)
			}
			require.ElementsMatch(t, tc.want, seen)
		})
	}
}

// Defect: a crash between the row delete and the synchronous vector delete
// leaked vectors for good, because their ids existed only in memory. A task
// carrying OnlyIfDeleted is enqueued before the DELETE and covers that window.
func TestFAQReplaceEnqueuesIndexDeleteBeforeDeletingRows(t *testing.T) {
	var events []string
	enqueuer := &eventRecordingEnqueuer{events: &events}
	repo := &orderedDeleteChunkRepo{
		events:     &events,
		planned:    []*types.Chunk{{ID: "c1", TenantID: 1}, {ID: "c2", TenantID: 1}},
		deletedIDs: []string{"c1", "c2"},
	}
	s := &knowledgeService{
		chunkRepo:      repo,
		modelService:   faqVectorEmbeddings{embedding: &faqVectorEmbedder{}},
		retrieveEngine: recordingVectorRegistry{engine: &recordingVectorEngine{}},
		repo:           &faqVectorKnowledgeRepo{},
		task:           enqueuer,
	}
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model", Type: types.KnowledgeBaseTypeFAQ}

	require.NoError(t, s.deleteReplacedFAQChunks(faqVectorFallbackContext(), "task-1", 1, kb,
		&types.Knowledge{ID: "knowledge", TenantID: 1}, repo.planned))

	require.Equal(t, []string{"recheck", "enqueue", "delete"}, events,
		"the crash-window task is queued before the rows are deleted")
	require.Len(t, enqueuer.tasks, 1)
	var payload types.IndexDeletePayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.True(t, payload.OnlyIfDeleted, "the pre-delete task must only touch rows the delete reached")
	require.ElementsMatch(t, []string{"c1", "c2"}, payload.ChunkIDs)
}

// A delete that fails after committing earlier batches reports the ids it did
// delete; they get their own flagged task as well.
func TestFAQReplaceDeleteFailureEnqueuesReturnedIds(t *testing.T) {
	var events []string
	enqueuer := &eventRecordingEnqueuer{events: &events}
	repo := &orderedDeleteChunkRepo{
		events:     &events,
		planned:    []*types.Chunk{{ID: "c1", TenantID: 1}, {ID: "c2", TenantID: 1}},
		deletedIDs: []string{"c1"},
		deleteErr:  errors.New("connection reset"),
	}
	s := &knowledgeService{
		chunkRepo:      repo,
		modelService:   faqVectorEmbeddings{embedding: &faqVectorEmbedder{}},
		retrieveEngine: recordingVectorRegistry{engine: &recordingVectorEngine{}},
		repo:           &faqVectorKnowledgeRepo{},
		task:           enqueuer,
	}
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model", Type: types.KnowledgeBaseTypeFAQ}

	err := s.deleteReplacedFAQChunks(faqVectorFallbackContext(), "task-1", 1, kb,
		&types.Knowledge{ID: "knowledge", TenantID: 1}, repo.planned)
	require.Error(t, err)

	require.Len(t, enqueuer.tasks, 2, "the pre-delete coverage plus the committed ids")
	ids := make([][]string, 0, 2)
	for _, task := range enqueuer.tasks {
		var payload types.IndexDeletePayload
		require.NoError(t, json.Unmarshal(task.Payload(), &payload))
		require.True(t, payload.OnlyIfDeleted)
		ids = append(ids, payload.ChunkIDs)
	}
	require.ElementsMatch(t, []string{"c1", "c2"}, ids[0])
	require.Equal(t, []string{"c1"}, ids[1])
}

type eventRecordingEnqueuer struct {
	interfaces.TaskEnqueuer
	mu     sync.Mutex
	events *[]string
	tasks  []*asynq.Task
}

func (e *eventRecordingEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.events = append(*e.events, "enqueue")
	e.tasks = append(e.tasks, task)
	return &asynq.TaskInfo{ID: "task"}, nil
}

type orderedDeleteChunkRepo struct {
	interfaces.ChunkRepository
	events     *[]string
	planned    []*types.Chunk
	deletedIDs []string
	deleteErr  error
}

func (r *orderedDeleteChunkRepo) UnchangedChunks(
	_ context.Context, _ uint64, _ []*types.Chunk,
) ([]*types.Chunk, error) {
	*r.events = append(*r.events, "recheck")
	return r.planned, nil
}

func (r *orderedDeleteChunkRepo) DeleteChunksIfUnchanged(
	_ context.Context, _ uint64, _ []*types.Chunk,
) ([]string, error) {
	*r.events = append(*r.events, "delete")
	return r.deletedIDs, r.deleteErr
}

// Defect: finalization deleted the entries file before writing the result, so
// a failed write left a task that could not be retried.
func TestFinalizeDeletesEntriesFileOnlyAfterTheResultIsPersisted(t *testing.T) {
	f := newFAQWriteFixture(t)
	var events []string
	f.files.events = &events
	f.svc.repo = &faqKnowledgeRepoSpy{KnowledgeRepository: f.svc.repo, events: &events}

	payload := &types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-finalize",
		Mode: types.FAQBatchModeReplace, EntriesURL: "mem/entries.json",
	}
	progress := &types.FAQImportProgress{
		TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
		TenantID: 7, Status: types.FAQImportStatusProcessing, Total: 1, CreatedAt: 1,
	}
	require.NoError(t, f.svc.finalizeFAQValidation(f.ctx, payload, progress, 1))
	require.Contains(t, events, "db-update")
	require.Contains(t, events, "file-delete")
	require.Less(t, indexOf(events, "db-update"), indexOf(events, "file-delete"),
		"the retry payload is deleted only after the result is in the database")
}

func TestFinalizeKeepsTheEntriesFileWhenTheResultWriteFails(t *testing.T) {
	f := newFAQWriteFixture(t)
	var events []string
	f.files.events = &events
	f.svc.repo = &faqKnowledgeRepoSpy{
		KnowledgeRepository: f.svc.repo,
		updateErr:           errors.New("database unavailable"),
		events:              &events,
	}

	payload := &types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-finalize",
		Mode: types.FAQBatchModeReplace, EntriesURL: "mem/entries.json",
	}
	progress := &types.FAQImportProgress{
		TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
		TenantID: 7, Status: types.FAQImportStatusProcessing, Total: 1, CreatedAt: 1,
	}
	require.Error(t, f.svc.finalizeFAQValidation(f.ctx, payload, progress, 1))
	require.Empty(t, f.files.deleted, "the retry still needs the entries file")
	require.NotContains(t, events, "file-delete")
}

// Defect: finalization could fail on the last attempt and leave the record at
// "processing" forever.
func TestFinalizeFailureOnTheLastRetryClosesTheRecord(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.repo = &faqKnowledgeRepoSpy{
		KnowledgeRepository: f.svc.repo,
		updateErr:           errors.New("database unavailable"),
	}
	payload := types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-finish",
		InstanceID: "inst-finish", Mode: types.FAQBatchModeReplace,
		Entries: []types.FAQEntryPayload{{
			StandardQuestion: "How do I reset my password?",
			Answers:          []string{"Use the reset link."},
			AnswerStrategy:   faqStrategy(types.AnswerStrategy("ALL")),
		}},
	}
	require.NoError(t, f.svc.saveFAQImportProgress(f.ctx, &types.FAQImportProgress{
		TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
		TenantID: 7, InstanceID: payload.InstanceID, Status: types.FAQImportStatusPending, Total: 1,
	}))

	err := f.svc.ProcessFAQImport(f.ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload)))
	require.Error(t, err, "the failed result write is still reported")

	progress, getErr := f.svc.GetFAQImportProgress(f.ctx, payload.TaskID)
	require.NoError(t, getErr)
	require.Equal(t, types.FAQImportStatusFailed, progress.Status,
		"a terminal finalization failure must not leave the record processing")
	require.NotEmpty(t, progress.Error)
}

// Defect: a download failure on the last retry left the task "processing"
// forever, because the error was returned without closing the record.
func TestDownloadFailureOnTheLastRetryClosesTheRecord(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.files.getErr = errors.New("object store unavailable")
	payload := types.FAQImportPayload{
		TenantID: 7, KBID: "kb", KnowledgeID: "faq", TaskID: "task-download",
		InstanceID: "inst-download", Mode: types.FAQBatchModeReplace,
		EntriesURL: "mem/entries.json",
	}
	require.NoError(t, f.svc.saveFAQImportProgress(f.ctx, &types.FAQImportProgress{
		TaskID: payload.TaskID, KBID: payload.KBID, KnowledgeID: payload.KnowledgeID,
		TenantID: 7, InstanceID: payload.InstanceID, Status: types.FAQImportStatusPending, Total: 3,
	}))

	err := f.svc.ProcessFAQImport(f.ctx, asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload)))
	require.Error(t, err)
	require.NotErrorIs(t, err, asynq.SkipRetry, "a transient download failure stays retryable")

	progress, getErr := f.svc.GetFAQImportProgress(f.ctx, payload.TaskID)
	require.NoError(t, getErr)
	require.Equal(t, types.FAQImportStatusFailed, progress.Status)
	require.Contains(t, progress.Message, "下载", "the record says why the task stopped")
	require.Contains(t, progress.Error, "object store unavailable")
}

// Defect: progress records were keyed by task id alone, so a client-chosen id
// let one tenant read or overwrite another tenant's record.
func TestFAQProgressIsTenantScoped(t *testing.T) {
	scenarios := map[string]func(t *testing.T) *knowledgeService{
		"in-memory": func(t *testing.T) *knowledgeService { return newFAQWriteFixture(t).svc },
		"redis": func(t *testing.T) *knowledgeService {
			f := newFAQWriteFixture(t)
			f.svc.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			t.Cleanup(func() { _ = f.svc.redisClient.Close() })
			return f.svc
		},
	}
	for name, build := range scenarios {
		t.Run(name, func(t *testing.T) {
			svc := build(t)
			ctxA := types.WithExecutionTenant(context.Background(), 7)
			ctxB := types.WithExecutionTenant(context.Background(), 8)

			require.NoError(t, svc.writeFAQImportProgress(ctxA, &types.FAQImportProgress{
				TaskID: "shared-id", TenantID: 7, Status: types.FAQImportStatusCompleted, Message: "tenant A",
			}))

			_, err := svc.GetFAQImportProgress(ctxB, "shared-id")
			require.Error(t, err, "another tenant's task id is not readable")

			require.NoError(t, svc.writeFAQImportProgress(ctxB, &types.FAQImportProgress{
				TaskID: "shared-id", TenantID: 8, Status: types.FAQImportStatusProcessing, Message: "tenant B",
			}))
			a, err := svc.GetFAQImportProgress(ctxA, "shared-id")
			require.NoError(t, err)
			require.Equal(t, "tenant A", a.Message, "tenant B's write must not overwrite tenant A's record")
			b, err := svc.GetFAQImportProgress(ctxB, "shared-id")
			require.NoError(t, err)
			require.Equal(t, "tenant B", b.Message)
			require.Equal(t, uint64(8), b.TenantID)
		})
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}
