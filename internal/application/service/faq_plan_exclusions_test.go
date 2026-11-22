package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// excludeRecordingChunks records the ids the planner asks the repository for,
// so a test can see what the exclusion list actually costs.
type excludeRecordingChunks struct {
	interfaces.ChunkRepository
	asked  []int64
	chunks []*types.Chunk
}

func (r *excludeRecordingChunks) ListChunksBySeqID(_ context.Context, _ uint64, seqIDs []int64) ([]*types.Chunk, error) {
	r.asked = append(r.asked, seqIDs...)
	var out []*types.Chunk
	for _, chunk := range r.chunks {
		for _, id := range seqIDs {
			if chunk.SeqID == id {
				out = append(out, chunk)
			}
		}
	}
	return out, nil
}

// stubTags answers the tag lookup the by_tag path makes with one in-scope tag.
type stubTags struct {
	interfaces.KnowledgeTagRepository
}

func (s stubTags) GetBySeqIDs(_ context.Context, _ uint64, seqIDs []int64) ([]*types.KnowledgeTag, error) {
	out := make([]*types.KnowledgeTag, 0, len(seqIDs))
	for _, id := range seqIDs {
		out = append(out, &types.KnowledgeTag{SeqID: id, TenantID: 7, KnowledgeBaseID: "kb-1"})
	}
	return out, nil
}

func faqPlanFixture(ids ...int64) ([]*types.Chunk, *types.KnowledgeBase) {
	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 7}
	chunks := make([]*types.Chunk, 0, len(ids))
	for _, id := range ids {
		chunks = append(chunks, &types.Chunk{
			ID:              "c-" + string(rune('a'+id)),
			SeqID:           id,
			TenantID:        kb.TenantID,
			KnowledgeBaseID: kb.ID,
			ChunkType:       types.ChunkTypeFAQ,
		})
	}
	return chunks, kb
}

// Without a by_tag nothing reads the exclusions: the planned UUIDs are handed
// only to the tag update, and no row they name is excluded from anything. So
// the planner must not spend a lookup per id on them either -- otherwise the
// cap that was relaxed for exactly this case would not matter, because the
// request would still pay for every id in the list.
func TestPlannerIgnoresExclusionsWithoutByTag(t *testing.T) {
	chunks, kb := faqPlanFixture(1, 2, 3)
	repo := &excludeRecordingChunks{chunks: chunks}
	svc := &knowledgeService{chunkRepo: repo, tagRepo: stubTags{}}

	plan, err := svc.planFAQFields(t.Context(), kb, &types.FAQEntryFieldsBatchUpdate{
		ByID:       map[int64]types.FAQEntryFieldsUpdate{1: {}},
		ExcludeIDs: []int64{2, 3},
	})
	require.NoError(t, err)
	require.Equal(t, []int64{1}, repo.asked, "only the by_id target should be looked up")
	require.Empty(t, plan.excludeIDs, "the inert list must not become exclusions")
	require.Contains(t, plan.chunks, int64(1))
}

// With a by_tag the list is load-bearing: every row it fails to name is a row
// the tag update changes, so the planner has to resolve and check all of them.
func TestPlannerLoadsAndChecksExclusionsWithByTag(t *testing.T) {
	chunks, kb := faqPlanFixture(1, 2, 3)
	repo := &excludeRecordingChunks{chunks: chunks}
	svc := &knowledgeService{chunkRepo: repo, tagRepo: stubTags{}}

	plan, err := svc.planFAQFields(t.Context(), kb, &types.FAQEntryFieldsBatchUpdate{
		ByTag:      map[int64]types.FAQEntryFieldsUpdate{5: {}},
		ExcludeIDs: []int64{2, 3},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{2, 3}, repo.asked, "every exclusion has to be checked")
	require.Len(t, plan.excludeIDs, 2)
}

// An exclusion that names a row outside the knowledge base must still stop a
// by_tag update: the tag path would otherwise change a row the caller never
// had checked.
func TestPlannerRejectsForeignExclusionsWithByTag(t *testing.T) {
	chunks, kb := faqPlanFixture(1, 2)
	chunks[1].KnowledgeBaseID = "kb-other"
	repo := &excludeRecordingChunks{chunks: chunks}
	svc := &knowledgeService{chunkRepo: repo, tagRepo: stubTags{}}

	_, err := svc.planFAQFields(t.Context(), kb, &types.FAQEntryFieldsBatchUpdate{
		ByTag:      map[int64]types.FAQEntryFieldsUpdate{5: {}},
		ExcludeIDs: []int64{2},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "不属于当前知识库")
}
