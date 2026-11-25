package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

// A replace-mode import plans its deletions from a snapshot and only re-checks
// ownership afterwards; a row another writer rewrote between the two must not
// be deleted by the import. The fence is the identity the plan selected the row
// by: seq id and content hash.
func TestDeleteChunksIfUnchangedLeavesRowsThatMovedSinceThePlan(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	for _, chunk := range []*types.Chunk{
		{ID: "same", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h1"},
		{ID: "rewritten", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h1"},
		{ID: "other-tenant", TenantID: 8, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h1"},
	} {
		require.NoError(t, db.Create(chunk).Error)
	}
	// The plan was computed while "rewritten" still carried h1.
	planned := []*types.Chunk{
		plannedFromDB(t, db, "same"),
		plannedFromDB(t, db, "rewritten"),
		plannedFromDB(t, db, "other-tenant"),
	}
	// A user edits "rewritten" after the plan was taken.
	require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", "rewritten").
		Update("content_hash", "h2").Error)

	unchanged, err := repo.UnchangedChunks(ctx, 7, planned)
	require.NoError(t, err)
	ids := make([]string, 0, len(unchanged))
	for _, c := range unchanged {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []string{"same"}, ids, "the rewritten row and the other tenant's row are not ours to touch")

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.Equal(t, []string{"same"}, removed,
		"the result names exactly the rows the caller may now delete vectors for")
	_, err = repo.GetChunkByID(ctx, 7, "rewritten")
	require.NoError(t, err, "the row the concurrent write touched must survive")
	_, err = repo.GetChunkByID(ctx, 7, "same")
	require.Error(t, err, "the unchanged row is deleted")
	_, err = repo.GetChunkByID(ctx, 8, "other-tenant")
	require.NoError(t, err, "another tenant's row is never in scope")
}

// A planned row that vanished before the delete is absent from the result: the
// caller must not remove vectors for a row it did not delete here (the row may
// have been replaced by another import that reindexed it).
func TestDeleteChunksIfUnchangedToleratesRowsThatVanished(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	require.NoError(t, db.Create(&types.Chunk{
		ID: "live", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h",
	}).Error)
	planned := []*types.Chunk{
		plannedFromDB(t, db, "live"),
		{ID: "gone", SeqID: 42, ContentHash: "h"},
	}

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.Equal(t, []string{"live"}, removed)
}

// A row whose identity columns were never assigned stores NULL for seq id and
// content hash (Lite/SQLite deployments have no sequence). The plan reads them
// back as zero values, and the fence must still match: `seq_id = 0` alone would
// not.
func TestDeleteChunksIfUnchangedMatchesUnassignedIdentity(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	require.NoError(t, db.Create(&types.Chunk{
		ID: "null-identity", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ,
	}).Error)

	planned := plannedFromDB(t, db, "null-identity")
	require.Zero(t, planned.SeqID)
	require.Empty(t, planned.ContentHash)

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, []*types.Chunk{planned})
	require.NoError(t, err)
	require.Equal(t, []string{"null-identity"}, removed,
		"an unassigned identity must still be deletable")
}

// The import cap is configurable, so the planned row list is not bounded by
// anything this repository controls. Both fence statements walk it in batches;
// the verdict must be the same as if it were one statement, including rows that
// changed on either side of a batch boundary.
func TestChunkFenceBatchesGiveTheSameVerdictPerRow(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	const total = chunkFenceBatchSize*2 + 7
	rows := make([]*types.Chunk, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("c%03d", i)
		rows = append(rows, &types.Chunk{
			ID: id, TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h",
		})
	}
	require.NoError(t, db.CreateInBatches(rows, 100).Error)
	planned := make([]*types.Chunk, 0, total)
	for _, row := range rows {
		planned = append(planned, plannedFromDB(t, db, row.ID))
	}

	// Rewrite one row inside the first batch, one exactly at the boundary, and
	// one in the trailing partial batch.
	moved := map[string]bool{"c005": true}
	for i := 0; i < len(planned); i += chunkFenceBatchSize {
		moved[planned[i].ID] = true
	}
	for id := range moved {
		require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).
			Update("content_hash", "rewritten").Error)
	}

	unchanged, err := repo.UnchangedChunks(ctx, 7, planned)
	require.NoError(t, err)
	require.Len(t, unchanged, total-len(moved), "every batch is checked, none is dropped")

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.Len(t, removed, total-len(moved), "the delete walk covers the same rows")
	for id := range moved {
		require.NotContains(t, removed, id, "a row that moved is never deleted")
		_, err := repo.GetChunkByID(ctx, 7, id)
		require.NoError(t, err, "and it survives the delete")
	}
}

// plannedFromDB builds the row a replace import would have put in its plan: the
// identity columns ListAllFAQChunksByKnowledgeID selects, as the database holds
// them.
func plannedFromDB(t *testing.T, db *gorm.DB, id string) *types.Chunk {
	t.Helper()
	var chunk types.Chunk
	require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).First(&chunk).Error)
	return &types.Chunk{
		ID:          chunk.ID,
		SeqID:       chunk.SeqID,
		ContentHash: chunk.ContentHash,
		TagID:       chunk.TagID,
		IsEnabled:   chunk.IsEnabled,
		Flags:       chunk.Flags,
		Metadata:    chunk.Metadata,
	}
}

// The answer strategy lives in the metadata JSON, which the content hash does
// not cover. A replace plan inherits it for entries that omit it, so a row whose
// strategy changed after the plan was taken must be left alone -- otherwise the
// plan's stale copy overwrites the edit when the entry is re-created.
func TestDeleteChunksIfUnchangedLeavesRowsWhoseAnswerStrategyMoved(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	meta := func(strategy string) types.JSON {
		return types.JSON(fmt.Sprintf(`{"standard_question":"q","answers":["a"],"answer_strategy":%q}`, strategy))
	}
	for _, chunk := range []*types.Chunk{
		{ID: "same", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h", Metadata: meta("all")},
		{ID: "restrategised", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h", Metadata: meta("all")},
	} {
		require.NoError(t, db.Create(chunk).Error)
	}
	planned := []*types.Chunk{plannedFromDB(t, db, "same"), plannedFromDB(t, db, "restrategised")}
	require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", "restrategised").
		Update("metadata", meta("random")).Error)

	unchanged, err := repo.UnchangedChunks(ctx, 7, planned)
	require.NoError(t, err)
	require.Len(t, unchanged, 1)
	require.Equal(t, "same", unchanged[0].ID)

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.Equal(t, []string{"same"}, removed,
		"the row whose answer strategy changed is not ours to replace")
	_, err = repo.GetChunkByID(ctx, 7, "restrategised")
	require.NoError(t, err, "and it survives the delete")
}

// A plugin edit that only moves a tag (or flips enabled / recommended) leaves
// the content hash untouched, because the hash covers the question and answers
// only. The plan would then match a row the user just categorised or disabled,
// and deleting it would discard that edit -- the fence has to carry the whole
// identity the plan read.
func TestDeleteChunksIfUnchangedLeavesRowsWhoseTagEnabledOrFlagsMoved(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(db *gorm.DB, id string)
	}{
		{"tag", func(db *gorm.DB, id string) {
			require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).Update("tag_id", "new-tag").Error)
		}},
		{"enabled", func(db *gorm.DB, id string) {
			require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).Update("is_enabled", false).Error)
		}},
		{"flags", func(db *gorm.DB, id string) {
			require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).Update("flags", 0).Error)
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			db := setupChunkTestDB(t)
			repo := NewChunkRepository(db)
			ctx := context.Background()

			require.NoError(t, db.Create(&types.Chunk{
				ID: "entry", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ,
				ContentHash: "h", IsEnabled: true, Flags: types.ChunkFlagRecommended,
			}).Error)
			planned := []*types.Chunk{plannedFromDB(t, db, "entry")}

			change.apply(db, "entry")

			unchanged, err := repo.UnchangedChunks(ctx, 7, planned)
			require.NoError(t, err)
			require.Empty(t, unchanged, "the row moved since the plan; it is not the import's to delete")

			removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
			require.NoError(t, err)
			require.Empty(t, removed)
			_, err = repo.GetChunkByID(ctx, 7, "entry")
			require.NoError(t, err, "the concurrent edit survives")
		})
	}
}

// The read-back after the delete is scoped by tenant and id, so it also sees a
// row another writer soft-deleted in the same window. That is the intended
// behaviour, not a leak: the row is dead either way, the writer that deleted it
// removes its vectors on its own path, and deleting already-dead vectors twice
// is harmless. This test pins that reading so a future change to the read-back
// is a deliberate decision rather than an accident.
func TestDeleteChunksIfUnchangedReadBackCoversRowsAnotherWriterDeleted(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	require.NoError(t, db.Create(&types.Chunk{
		ID: "ours", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h",
	}).Error)
	require.NoError(t, db.Create(&types.Chunk{
		ID: "theirs", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, ContentHash: "h",
	}).Error)
	planned := []*types.Chunk{
		plannedFromDB(t, db, "ours"),
		plannedFromDB(t, db, "theirs"),
	}

	// Another writer soft-deletes one planned row before this delete runs.
	require.NoError(t, db.Where("id = ?", "theirs").Delete(&types.Chunk{}).Error)

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"ours", "theirs"}, removed,
		"both rows are unreachable afterwards; the caller may clear vectors for either")
	_, err = repo.GetChunkByID(ctx, 7, "theirs")
	require.Error(t, err, "neither row is live")
}
