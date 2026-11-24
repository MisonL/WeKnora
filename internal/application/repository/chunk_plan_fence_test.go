package repository

import (
	"context"
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
		{ID: "same", SeqID: seqIDOf(t, db, "same"), ContentHash: "h1"},
		{ID: "rewritten", SeqID: seqIDOf(t, db, "rewritten"), ContentHash: "h1"},
		{ID: "other-tenant", SeqID: seqIDOf(t, db, "other-tenant"), ContentHash: "h1"},
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
		{ID: "live", SeqID: seqIDOf(t, db, "live"), ContentHash: "h"},
		{ID: "gone", SeqID: 42, ContentHash: "h"},
	}

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, planned)
	require.NoError(t, err)
	require.Equal(t, []string{"live"}, removed)
}

// A row whose seq id and content hash were never assigned stores NULL for both
// (Lite/SQLite deployments have no sequence). The plan reads them back as zero
// values, and the fence must still match: `seq_id = 0` alone would not.
func TestDeleteChunksIfUnchangedMatchesUnassignedIdentity(t *testing.T) {
	db := setupChunkTestDB(t)
	repo := NewChunkRepository(db)
	ctx := context.Background()

	require.NoError(t, db.Create(&types.Chunk{
		ID: "null-identity", TenantID: 7, KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ,
	}).Error)

	removed, err := repo.DeleteChunksIfUnchanged(ctx, 7, []*types.Chunk{
		{ID: "null-identity", SeqID: 0, ContentHash: ""},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"null-identity"}, removed,
		"an unassigned identity must still be deletable")
}

func seqIDOf(t *testing.T, db *gorm.DB, id string) int64 {
	t.Helper()
	var chunk types.Chunk
	require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", id).First(&chunk).Error)
	return chunk.SeqID
}
