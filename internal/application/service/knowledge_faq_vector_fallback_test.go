package service

// A replace-mode import soft-deletes the replaced rows first and removes their
// vectors second. If the vector delete fails, the rows are already gone, so the
// import's own retry recomputes its plan from live rows and never lists them
// again: without a fallback the vectors would leak for good. They are handed to
// the durable index-delete task instead.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type recordingEnqueuer struct {
	interfaces.TaskEnqueuer
	mu    sync.Mutex
	tasks []*asynq.Task
}

func (e *recordingEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, task)
	return &asynq.TaskInfo{ID: "task"}, nil
}

type faqVectorEmbeddings struct {
	interfaces.ModelService
	embedding embedding.Embedder
}

func (m faqVectorEmbeddings) GetEmbeddingModel(context.Context, string) (embedding.Embedder, error) {
	return m.embedding, nil
}

type faqVectorEmbedder struct{ embedding.Embedder }

func (*faqVectorEmbedder) GetDimensions() int { return 2 }

type faqVectorEngine struct {
	interfaces.RetrieveEngineService
	err error
}

func (*faqVectorEngine) EngineType() types.RetrieverEngineType { return types.SQLiteRetrieverEngineType }

func (*faqVectorEngine) Support() []types.RetrieverType { return []types.RetrieverType{types.VectorRetrieverType} }

func (*faqVectorEngine) EstimateStorageSize(context.Context, embedding.Embedder, []*types.IndexInfo, []types.RetrieverType) int64 {
	return 0
}

func (e *faqVectorEngine) DeleteByChunkIDList(context.Context, []string, int, string) error {
	return e.err
}

type faqVectorKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	updated []*types.Knowledge
}

func (r *faqVectorKnowledgeRepo) UpdateKnowledge(_ context.Context, knowledge *types.Knowledge) error {
	r.updated = append(r.updated, knowledge)
	return nil
}

type faqVectorRegistry struct {
	interfaces.RetrieveEngineRegistry
	engine *faqVectorEngine
}

func (r faqVectorRegistry) GetRetrieveEngineService(
	types.RetrieverEngineType,
) (interfaces.RetrieveEngineService, error) {
	return r.engine, nil
}

func setupFAQVectorFallback(t *testing.T, vectorErr error) (*knowledgeService, *recordingEnqueuer, []*types.Chunk) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	chunkRepo := repository.NewChunkRepository(db)

	rows := []*types.Chunk{
		{ID: "c1", TenantID: 1, KnowledgeBaseID: "kb", KnowledgeID: "knowledge",
			ChunkType: types.ChunkTypeFAQ, Content: "q1", ContentHash: "h1", IsEnabled: true},
		{ID: "c2", TenantID: 1, KnowledgeBaseID: "kb", KnowledgeID: "knowledge",
			ChunkType: types.ChunkTypeFAQ, Content: "q2", ContentHash: "h2", IsEnabled: true},
	}
	require.NoError(t, chunkRepo.CreateChunks(context.Background(), rows))
	planned := make([]*types.Chunk, 0, len(rows))
	for _, row := range rows {
		stored, err := chunkRepo.GetChunkByID(context.Background(), 1, row.ID)
		require.NoError(t, err)
		planned = append(planned, stored)
	}

	enqueuer := &recordingEnqueuer{}
	s := &knowledgeService{
		chunkRepo:      chunkRepo,
		modelService:   faqVectorEmbeddings{embedding: &faqVectorEmbedder{}},
		retrieveEngine: faqVectorRegistry{engine: &faqVectorEngine{err: vectorErr}},
		task:           enqueuer,
		repo:           &faqVectorKnowledgeRepo{},
	}
	return s, enqueuer, planned
}

// faqVectorFallbackContext carries the tenant the vector delete resolves its
// engine and storage refund from.
func faqVectorFallbackContext() context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	return context.WithValue(ctx, types.TenantInfoContextKey, &types.Tenant{
		ID: 1,
		RetrieverEngines: types.RetrieverEngines{
			Engines: []types.RetrieverEngineParams{{
				RetrieverType:       types.VectorRetrieverType,
				RetrieverEngineType: types.SQLiteRetrieverEngineType,
			}},
		},
	})
}

func TestDeleteReplacedFAQChunksHandsOrphanedVectorsToTheIndexTask(t *testing.T) {
	s, enqueuer, planned := setupFAQVectorFallback(t, errors.New("vector store unavailable"))
	ctx := faqVectorFallbackContext()
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model", Type: types.KnowledgeBaseTypeFAQ}
	faqKnowledge := &types.Knowledge{ID: "knowledge", TenantID: 1, KnowledgeBaseID: "kb"}

	err := s.deleteReplacedFAQChunks(ctx, "task-1", 1, kb, faqKnowledge, planned)
	require.Error(t, err, "the import still reports the failed vector delete")
	require.Contains(t, err.Error(), "failed to delete chunk vectors")

	require.Len(t, enqueuer.tasks, 1, "the orphaned vectors must not be left for a plan that cannot see them")
	task := enqueuer.tasks[0]
	require.Equal(t, types.TypeIndexDelete, task.Type())
	var payload types.IndexDeletePayload
	require.NoError(t, json.Unmarshal(task.Payload(), &payload))
	require.Equal(t, uint64(1), payload.TenantID, "the fallback runs under the tenant that owns the rows")
	require.Equal(t, "kb", payload.KnowledgeBaseID)
	require.Equal(t, "model", payload.EmbeddingModelID, "the worker needs the dimensions of the model that indexed them")
	require.Equal(t, types.KnowledgeTypeFAQ, payload.KBType)
	require.ElementsMatch(t, []string{"c1", "c2"}, payload.ChunkIDs,
		"exactly the rows this call soft-deleted")
}

func TestDeleteReplacedFAQChunksSkipsTheFallbackWhenVectorsDeleteCleanly(t *testing.T) {
	s, enqueuer, planned := setupFAQVectorFallback(t, nil)
	ctx := faqVectorFallbackContext()
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, EmbeddingModelID: "model", Type: types.KnowledgeBaseTypeFAQ}
	faqKnowledge := &types.Knowledge{ID: "knowledge", TenantID: 1, KnowledgeBaseID: "kb"}

	require.NoError(t, s.deleteReplacedFAQChunks(ctx, "task-1", 1, kb, faqKnowledge, planned))
	require.Empty(t, enqueuer.tasks, "nothing to hand over when the vectors are already gone")
}
