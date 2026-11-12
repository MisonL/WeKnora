package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// seedFAQChunk 插入一条带 metadata 的 FAQ 条目，供 append 模式识别合并目标。
func seedFAQChunk(t *testing.T, f *faqWriteFixture, id string, seqID int64, meta *types.FAQChunkMetadata) {
	t.Helper()
	chunk := &types.Chunk{
		ID:              id,
		SeqID:           seqID,
		TenantID:        7,
		KnowledgeBaseID: "kb",
		KnowledgeID:     "faq",
		ChunkType:       types.ChunkTypeFAQ,
		TagID:           "tag",
		IsEnabled:       true,
		Flags:           types.ChunkFlagRecommended,
		// 合并检测只扫描已完成索引的 FAQ 块（status = indexed）。
		Status: int(types.ChunkStatusIndexed),
	}
	require.NoError(t, chunk.SetFAQMetadata(meta))
	require.NoError(t, f.db.Create(chunk).Error)
}

// faqTestEngine 补齐父引擎没有实现的删除方法。父类型嵌入的是 nil 接口，
// 合并路径会按 chunk 删除旧索引，直接调用会 panic。
type faqTestEngine struct {
	parentChildRetrieveEngine
	deletedSourceIDs []string
	deletedChunkIDs  []string
}

func (e *faqTestEngine) DeleteBySourceIDList(
	_ context.Context, sourceIDs []string, _ int, _ string,
) error {
	e.deletedSourceIDs = append(e.deletedSourceIDs, sourceIDs...)
	return nil
}

func (e *faqTestEngine) DeleteByChunkIDList(
	_ context.Context, indexIDList []string, _ int, _ string,
) error {
	e.deletedChunkIDs = append(e.deletedChunkIDs, indexIDList...)
	return nil
}

// useImportEngines 把 fixture 换成能真正建块、建索引的组件。
func useImportEngines(f *faqWriteFixture) *faqTestEngine {
	engine := &faqTestEngine{}
	f.svc.retrieveEngine = &fakeFanoutRegistry{
		byStore: map[string]interfaces.RetrieveEngineService{*f.kb.VectorStoreID: engine},
	}
	f.svc.modelService = parentChildModelService{embedder: parentChildEmbedder{}}
	f.svc.chunkService = &chunkService{
		chunkRepository: f.chunks, knowledgeRepo: f.svc.repo,
		kbRepository: &imageReaderKBRepo{kb: f.kb},
	}
	return engine
}

func TestFAQImportResumesFromNextEntryIndex(t *testing.T) {
	f := newFAQWriteFixture(t)
	useImportEngines(f)
	entries := make([]types.FAQEntryPayload, 5)
	for i := range entries {
		entries[i] = types.FAQEntryPayload{
			StandardQuestion: fmt.Sprintf("question %d", i),
			Answers:          []string{fmt.Sprintf("answer %d", i)},
		}
	}
	payload := &types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeAppend, Entries: entries}
	progress := &types.FAQImportProgress{TaskID: "resume", KBID: f.kb.ID}
	// 模拟首批已成功、任务中断后从第 3 条续跑：游标之前的下标不得重复处理。
	require.NoError(t, f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID, payload, f.kb.TenantID, 3, progress))

	var created []types.Chunk
	require.NoError(t, f.db.Where("knowledge_id = ? AND id <> ?", "faq", "one").Find(&created).Error)
	require.Len(t, created, 2)
	require.Equal(t, 5, progress.NextEntryIndex, "cursor must advance to the end of the plan")
	require.Len(t, progress.SuccessEntries, 2)
	require.Equal(t, []int{3, 4}, []int{progress.SuccessEntries[0].Index, progress.SuccessEntries[1].Index})
}

func TestFAQImportPlannedIndicesSurviveSkipsAndMerges(t *testing.T) {
	f := newFAQWriteFixture(t)
	useImportEngines(f)
	seedFAQChunk(t, f, "merged", 3, &types.FAQChunkMetadata{
		StandardQuestion: "existing question",
		Answers:          []string{"old answer"},
	})
	entries := []types.FAQEntryPayload{
		{StandardQuestion: "new question", Answers: []string{"a1"}},
		{StandardQuestion: "existing question", Answers: []string{"new answer"}},
		// 空标准问在预处理阶段被跳过，但仍占位，后面条目下标不得前移。
		{StandardQuestion: "", Answers: []string{"ignored"}},
		{StandardQuestion: "last question", Answers: []string{"a3"}},
	}
	progress := &types.FAQImportProgress{TaskID: "indices", KBID: f.kb.ID}
	require.NoError(t, f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID,
		&types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeAppend, Entries: entries}, f.kb.TenantID, 0, progress))

	require.Equal(t, 1, progress.MergedCount)
	require.Len(t, progress.MergeDetails, 1)
	require.Equal(t, 1, progress.MergeDetails[0].Index, "merge must report its original payload index")
	// 合并与新建都计入成功条目；跳过项不产生成功条目，但它的下标不能被复用。
	require.Len(t, progress.SuccessEntries, 3)
	byIndex := make(map[int]types.FAQSuccessEntry, len(progress.SuccessEntries))
	for _, entry := range progress.SuccessEntries {
		_, dup := byIndex[entry.Index]
		require.False(t, dup, "entry index %d reported twice", entry.Index)
		byIndex[entry.Index] = entry
	}
	require.Contains(t, byIndex, 0)
	require.Contains(t, byIndex, 1)
	require.Contains(t, byIndex, 3)
	require.NotContains(t, byIndex, 2, "skipped entry must not be reported as success")
	require.Equal(t, "last question", byIndex[3].StandardQuestion)
	require.Equal(t, 4, progress.NextEntryIndex)

	// 合并就地更新既有条目，不新增行：fixture 的 "one" + 合并目标 + 2 条新建。
	var all []types.Chunk
	require.NoError(t, f.db.Where("knowledge_id = ?", "faq").Find(&all).Error)
	require.Len(t, all, 4)
}

// failingRetrieveEngine 让第一次 BatchIndex 返回错误。不能用 panic 注入：
// CompositeRetrieveEngine 在独立 goroutine 里调用引擎，panic 直接终止进程，
// executeFAQImport 的 recover 接不到。
type failingRetrieveEngine struct {
	parentChildRetrieveEngine
	calls int
}

func (e *failingRetrieveEngine) DeleteBySourceIDList(context.Context, []string, int, string) error {
	return nil
}

func (e *failingRetrieveEngine) BatchIndex(
	_ context.Context, _ embedding.Embedder, infos []*types.IndexInfo, _ []types.RetrieverType,
) error {
	e.calls++
	if e.calls == 1 {
		return fmt.Errorf("engine unavailable")
	}
	e.indexed = append([]*types.IndexInfo(nil), infos...)
	return nil
}

func TestFAQImportFailureDoesNotAdvanceCursor(t *testing.T) {
	f := newFAQWriteFixture(t)
	engine := &failingRetrieveEngine{}
	f.svc.retrieveEngine = &fakeFanoutRegistry{
		byStore: map[string]interfaces.RetrieveEngineService{*f.kb.VectorStoreID: engine},
	}
	f.svc.modelService = parentChildModelService{embedder: parentChildEmbedder{}}
	f.svc.chunkService = &chunkService{
		chunkRepository: f.chunks, knowledgeRepo: f.svc.repo,
		kbRepository: &imageReaderKBRepo{kb: f.kb},
	}
	entries := []types.FAQEntryPayload{
		{StandardQuestion: "q0", Answers: []string{"a0"}},
		{StandardQuestion: "q1", Answers: []string{"a1"}},
	}
	progress := &types.FAQImportProgress{TaskID: "fail", KBID: f.kb.ID}
	err := f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID,
		&types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeAppend, Entries: entries}, f.kb.TenantID, 0, progress)
	require.ErrorContains(t, err, "engine unavailable")
	require.Zero(t, progress.NextEntryIndex, "a failed batch must not advance the retry cursor")
}

func TestIncrementalIndexRebuildsOnOperationalFieldChange(t *testing.T) {
	f := newFAQWriteFixture(t)
	engine := &faqTestEngine{}
	f.svc.retrieveEngine = &fakeFanoutRegistry{
		byStore: map[string]interfaces.RetrieveEngineService{*f.kb.VectorStoreID: engine},
	}
	f.svc.modelService = parentChildModelService{embedder: parentChildEmbedder{}}
	f.kb.FAQConfig = &types.FAQConfig{
		IndexMode:         types.FAQIndexModeQuestionAnswer,
		QuestionIndexMode: types.FAQQuestionIndexModeSeparate,
	}
	knowledge := &types.Knowledge{}
	require.NoError(t, f.db.First(knowledge, "id = ?", "faq").Error)
	meta := &types.FAQChunkMetadata{
		StandardQuestion: "how to reset password",
		SimilarQuestions: []string{"forgot my password"},
		Answers:          []string{"use the reset link"},
	}
	chunk := &types.Chunk{
		ID:              "op-chunk",
		SeqID:           9,
		TenantID:        7,
		KnowledgeID:     "faq",
		KnowledgeBaseID: "kb",
		ChunkType:       types.ChunkTypeFAQ,
		TagID:           "next",
		IsEnabled:       false,
		Flags:           0,
	}
	require.NoError(t, chunk.SetFAQMetadata(meta))

	// 内容完全没变，只有 tag / 启用位 / 推荐位变了。
	err := f.svc.incrementalIndexFAQEntry(f.ctx, f.kb, knowledge, chunk, parentChildEmbedder{},
		meta.StandardQuestion, meta.SimilarQuestions, meta.Answers,
		faqIndexOperationalState{TagID: "tag", IsEnabled: true, IsRecommended: true},
		meta.Normalize())
	require.NoError(t, err)

	require.Len(t, engine.indexed, 2, "standard and similar question must both be re-indexed")
	for _, info := range engine.indexed {
		require.Equal(t, "next", info.TagID)
		require.False(t, info.IsEnabled)
		require.False(t, info.IsRecommended)
	}
	// 标准问 + 相似问各一条，外加旧下标形式（op-chunk-0）的存量向量清理。
	require.Len(t, engine.deletedSourceIDs, 3, "stale vectors must be deleted before rewriting")
	require.Contains(t, engine.deletedSourceIDs, "op-chunk")
	require.Contains(t, engine.deletedSourceIDs, "op-chunk-0",
		"legacy index-based SourceID must be cleaned up on upgrade")
}
