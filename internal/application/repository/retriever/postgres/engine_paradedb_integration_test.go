package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Drives the real pgRepository against a real ParadeDB (the image production
// runs): BM25 keyword retrieval through the chinese_lindera tokenizer, halfvec
// vector retrieval against the app-built expression HNSW index, the upsert
// contract, and the enabled/tag filters -- none of which sqlite or miniredis
// can execute. Set WEKNORA_RETRIEVER_TEST_PARADEDB_DSN to a disposable
// database on a pg_search + pgvector server; the test creates its own
// embeddings table in a fresh schema and drops it afterwards.
func TestParadeDBEngineEndToEnd(t *testing.T) {
	dsn := os.Getenv("WEKNORA_RETRIEVER_TEST_PARADEDB_DSN")
	if dsn == "" {
		t.Skip("set WEKNORA_RETRIEVER_TEST_PARADEDB_DSN to run the ParadeDB engine tests")
	}
	t.Setenv("AUTO_MIGRATE", "true") // let the app build its HNSW index

	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	base, err := gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	schema := fmt.Sprintf("engine_%d", time.Now().UnixNano())
	require.NoError(t, base.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error)
	require.NoError(t, base.Exec("CREATE EXTENSION IF NOT EXISTS pg_search").Error)
	require.NoError(t, base.Exec("CREATE SCHEMA "+schema).Error)
	// public stays on the path so the extensions' types (installed there, as
	// in production) resolve; tables land in the fresh schema alone.
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema+",public"), config)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = base.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if sqlDB, err := base.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	// The production shape minus the indexes the app itself maintains: the
	// bm25 index is what migrations provide, but creating it here would hide
	// whether KeywordsRetrieve works against the migrated schema -- so it is
	// created exactly like 000002_embeddings does.
	require.NoError(t, db.Exec(`CREATE TABLE embeddings (
		id SERIAL PRIMARY KEY,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		source_id VARCHAR(64) NOT NULL,
		source_type INTEGER NOT NULL,
		chunk_id VARCHAR(64),
		knowledge_id VARCHAR(64),
		knowledge_base_id VARCHAR(64),
		tag_id VARCHAR(64),
		content TEXT,
		dimension INTEGER NOT NULL,
		embedding halfvec,
		is_enabled BOOLEAN DEFAULT TRUE
	)`).Error)
	require.NoError(t, db.Exec(
		"CREATE UNIQUE INDEX embeddings_unique_source ON embeddings(source_id, source_type)").Error)
	require.NoError(t, db.Exec(`CREATE INDEX embeddings_search_idx ON embeddings
		USING bm25 (id, knowledge_base_id, content, knowledge_id, chunk_id)
		WITH (key_field = 'id', text_fields = '{"content": {"tokenizer": {"type": "chinese_lindera"}}}')`).Error)

	repo := NewPostgresRetrieveEngineRepository(db).(*pgRepository)
	ctx := context.Background()

	embeddings := map[string][]float32{
		"src-a": {1, 0, 0, 0},
		"src-b": {0, 1, 0, 0},
		"src-c": {0.9, 0.1, 0, 0},
	}
	indexes := []*types.IndexInfo{
		{SourceID: "src-a", SourceType: types.ChunkSourceType, ChunkID: "c-a",
			KnowledgeID: "k-1", KnowledgeBaseID: "kb-1", Content: "收银机开不了机怎么办", IsEnabled: true},
		{SourceID: "src-b", SourceType: types.ChunkSourceType, ChunkID: "c-b",
			KnowledgeID: "k-1", KnowledgeBaseID: "kb-1", Content: "电子秤打印不了小票", IsEnabled: true},
		{SourceID: "src-c", SourceType: types.ChunkSourceType, ChunkID: "c-c",
			KnowledgeID: "k-1", KnowledgeBaseID: "kb-1", Content: "扫码枪没有反应", IsEnabled: false},
	}
	require.NoError(t, repo.BatchSave(ctx, indexes, map[string]any{"embedding": embeddings}))

	var stored []struct {
		SourceID string
		Enabled  bool
		Dim      int
	}
	require.NoError(t, db.Raw(
		"SELECT source_id, is_enabled AS enabled, dimension AS dim FROM embeddings ORDER BY source_id",
	).Scan(&stored).Error)
	require.Len(t, stored, 3)
	require.False(t, stored[2].Enabled, "the disabled flag must reach the row")
	require.Equal(t, 4, stored[0].Dim, "dimension derived from the embedding length")

	// The app built its expression HNSW index in the background; wait for it.
	require.Eventually(t, func() bool {
		var count int64
		err := db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'embeddings'
			AND indexdef LIKE '%USING hnsw%' AND indexdef LIKE '%halfvec(4)%'`).Scan(&count).Error
		return err == nil && count > 0
	}, 30*time.Second, 500*time.Millisecond, "ensureHNSWIndex has to build the partial index")

	// BM25 keyword retrieval with the production tokenizer.
	kw, err := repo.KeywordsRetrieve(ctx, types.RetrieveParams{
		RetrieverType:    types.KeywordsRetrieverType,
		Query:            "收银机",
		KnowledgeBaseIDs: []string{"kb-1"},
		TopK:             10,
	})
	require.NoError(t, err)
	require.Len(t, kw, 1)
	hits := sourceIDs(kw[0].Results)
	require.Equal(t, []string{"src-a"}, hits, "only the matching chunk may hit; a disabled row is filtered")

	// The disabled row is invisible to keyword search when re-enabled check
	// is what gates it: enable src-c and the query for its text hits.
	require.NoError(t, repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{"c-c": true}))
	kw, err = repo.KeywordsRetrieve(ctx, types.RetrieveParams{
		RetrieverType:    types.KeywordsRetrieverType,
		Query:            "扫码枪",
		KnowledgeBaseIDs: []string{"kb-1"},
		TopK:             10,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"src-c"}, sourceIDs(kw[0].Results))
	require.NoError(t, repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{"c-c": false}))

	// Tag filtering narrows keyword search (FAQ priority filtering path).
	require.NoError(t, repo.BatchUpdateChunkTagID(ctx, map[string]string{"c-a": "tag-1"}))
	kw, err = repo.KeywordsRetrieve(ctx, types.RetrieveParams{
		RetrieverType:    types.KeywordsRetrieverType,
		Query:            "收银机",
		KnowledgeBaseIDs: []string{"kb-1"},
		TagIDs:           []string{"tag-2"},
		TopK:             10,
	})
	require.NoError(t, err)
	require.Empty(t, sourceIDs(kw[0].Results), "a non-matching tag excludes the hit")

	// Vector retrieval through the HNSW path: the nearest neighbour first.
	vec, err := repo.VectorRetrieve(ctx, types.RetrieveParams{
		RetrieverType:    types.VectorRetrieverType,
		Embedding:        []float32{1, 0, 0, 0},
		KnowledgeBaseIDs: []string{"kb-1"},
		TopK:             5,
		Threshold:        0.5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, vec[0].Results)
	require.Equal(t, "src-a", vec[0].Results[0].SourceID, "cosine nearest neighbour ordering")
	require.InDelta(t, 1.0, vec[0].Results[0].Score, 1e-3)
	for _, r := range vec[0].Results {
		require.NotEqual(t, "src-c", r.SourceID, "a disabled chunk must not be recalled")
	}

	// Re-saving the same source upserts rather than duplicating.
	require.NoError(t, repo.Save(ctx, &types.IndexInfo{SourceID: "src-a", SourceType: types.ChunkSourceType,
		ChunkID: "c-a", KnowledgeID: "k-1", KnowledgeBaseID: "kb-1",
		Content: "收银机彻底黑屏", IsEnabled: true}, map[string]any{
		"embedding": map[string][]float32{"src-a": {0, 0, 1, 0}},
	}))
	var dup int64
	require.NoError(t, db.Raw("SELECT count(*) FROM embeddings WHERE source_id = 'src-a'").Scan(&dup).Error)
	require.Equal(t, int64(1), dup)

	// Deletion paths used by the import's replace flow.
	require.NoError(t, repo.DeleteByChunkIDList(ctx, []string{"c-a"}, 4, ""))
	require.NoError(t, repo.DeleteByKnowledgeIDList(ctx, []string{"k-1"}, 4, ""))
	var left int64
	require.NoError(t, db.Raw("SELECT count(*) FROM embeddings").Scan(&left).Error)
	require.Zero(t, left)
}

func sourceIDs(results []*types.IndexWithScore) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.SourceID)
	}
	return ids
}
