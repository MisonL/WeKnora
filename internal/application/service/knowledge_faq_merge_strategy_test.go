package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 导入文件里没有「答案策略」这一列时（导出的表格重新导入就是这样），条目
// 的 AnswerStrategy 是 nil，sanitizeFAQEntryPayload 会把它补成 all。如果
// 合并/替换路径把这个补出来的 all 当成用户意图，库里原来 random 的条目就
// 会被静默改回 all。省略必须是「沿用已存策略」。
func TestAppendMergeKeepsStoredStrategyWhenTheEntryOmitsIt(t *testing.T) {
	f := newFAQWriteFixture(t)
	useImportEngines(f)
	seedFAQChunk(t, f, "strategy-merge", 3, &types.FAQChunkMetadata{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"old answer"},
		AnswerStrategy:   types.AnswerStrategyRandom,
	})

	entries := []types.FAQEntryPayload{{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"new answer"},
		// AnswerStrategy 省略：导入表格没有这一列。
	}}
	progress := &types.FAQImportProgress{TaskID: "strategy-merge-task", KBID: f.kb.ID}
	require.NoError(t, f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID,
		&types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeAppend, Entries: entries}, f.kb.TenantID, 0, 0, progress))

	require.Equal(t, 1, progress.MergedCount)
	var chunk types.Chunk
	require.NoError(t, f.db.First(&chunk, "id = ?", "strategy-merge").Error)
	meta, err := chunk.FAQMetadata()
	require.NoError(t, err)
	require.Equal(t, types.AnswerStrategyRandom, meta.AnswerStrategy,
		"an omitted strategy column must not reset a stored random strategy")
	require.Equal(t, []string{"new answer"}, meta.Answers, "the answers themselves still follow the imported entry")
}

// 条目显式给出策略时，策略变更仍然必须生效——省略才沿用，不是一概忽略。
func TestAppendMergeAppliesAnExplicitStrategy(t *testing.T) {
	f := newFAQWriteFixture(t)
	useImportEngines(f)
	seedFAQChunk(t, f, "strategy-explicit", 3, &types.FAQChunkMetadata{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"old answer"},
		AnswerStrategy:   types.AnswerStrategyRandom,
	})

	all := types.AnswerStrategy(types.AnswerStrategyAll)
	entries := []types.FAQEntryPayload{{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"old answer"},
		AnswerStrategy:   &all,
	}}
	progress := &types.FAQImportProgress{TaskID: "strategy-explicit-task", KBID: f.kb.ID}
	require.NoError(t, f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID,
		&types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeAppend, Entries: entries}, f.kb.TenantID, 0, 0, progress))

	require.Equal(t, 1, progress.MergedCount)
	var chunk types.Chunk
	require.NoError(t, f.db.First(&chunk, "id = ?", "strategy-explicit").Error)
	meta, err := chunk.FAQMetadata()
	require.NoError(t, err)
	require.Equal(t, types.AnswerStrategyAll, meta.AnswerStrategy,
		"an explicit strategy has to win over the stored one")
}

// replace 模式下，同样内容的条目（省略策略列）不该因为「策略不同」被删除重建：
// 既有行原样保留，策略维持 random。
func TestReplaceKeepsStoredStrategyWhenTheEntryOmitsIt(t *testing.T) {
	f := newFAQWriteFixture(t)
	useImportEngines(f)
	seedMeta := &types.FAQChunkMetadata{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"same answer"},
		AnswerStrategy:   types.AnswerStrategyRandom,
	}
	seedFAQChunk(t, f, "strategy-replace", 3, seedMeta)
	// replace 模式按内容 hash 匹配既有行，seedFAQChunk 不写 hash（append 的
	// 合并按标准问匹配，用不到它），这里补上。
	require.NoError(t, f.db.Model(&types.Chunk{}).Where("id = ?", "strategy-replace").
		Update("content_hash", types.CalculateFAQContentHash(seedMeta)).Error)

	entries := []types.FAQEntryPayload{{
		StandardQuestion: "how do I reset my password",
		Answers:          []string{"same answer"},
		// 标签与既有行一致（fixture 里 seq_id 11 的就是 "tag"），否则标签变化
		// 本身就会触发删除重建，测不到「策略未变就不重建」这一点。
		TagID: 11,
	}}
	progress := &types.FAQImportProgress{TaskID: "strategy-replace-task", KBID: f.kb.ID}
	require.NoError(t, f.svc.executeFAQImport(f.ctx, progress.TaskID, f.kb.ID,
		&types.FAQBatchUpsertPayload{Mode: types.FAQBatchModeReplace, Entries: entries}, f.kb.TenantID, 0, 0, progress))

	var all []types.Chunk
	require.NoError(t, f.db.Where("knowledge_id = ?", "faq").Find(&all).Error)
	require.Len(t, all, 1, "an identical entry must not be deleted and re-created over its strategy")
	require.Equal(t, "strategy-replace", all[0].ID)
	meta, err := all[0].FAQMetadata()
	require.NoError(t, err)
	require.Equal(t, types.AnswerStrategyRandom, meta.AnswerStrategy)
}
