package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// The tombstone purge touches every table that keys on knowledge_id without a
// foreign key back to knowledges. These DDLs mirror the columns each delete
// filters on; the tables shared with the housekeeping suite come from
// setupHousekeepingDB.
const tombstoneChildDDL = `
CREATE TABLE IF NOT EXISTS chunks (
    id                VARCHAR(64) PRIMARY KEY,
    tenant_id         INTEGER NOT NULL DEFAULT 0,
    knowledge_id      VARCHAR(64),
    knowledge_base_id VARCHAR(64),
    content           TEXT,
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at        DATETIME
);
CREATE TABLE IF NOT EXISTS chunk_revisions (
    id           VARCHAR(64) PRIMARY KEY,
    tenant_id    INTEGER NOT NULL DEFAULT 0,
    knowledge_id VARCHAR(64),
    chunk_id     VARCHAR(64)
);
CREATE TABLE IF NOT EXISTS knowledge_tag_relations (
    knowledge_id VARCHAR(64) NOT NULL,
    tag_id       VARCHAR(64) NOT NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (knowledge_id, tag_id)
);
CREATE TABLE IF NOT EXISTS embeddings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    knowledge_id VARCHAR(64),
    chunk_id     VARCHAR(64)
);
`

func setupTombstoneDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	for _, ddl := range []string{
		knowledgeTestDDL, housekeepingSpansDDL, housekeepingPendingOpsDDL, tombstoneChildDDL,
	} {
		require.NoError(t, db.Exec(ddl).Error)
	}
	return db
}

// insertTombstone writes a soft-deleted knowledge row. GORM's default scope
// would hide it from most queries; raw SQL keeps the timestamps exact.
func insertTombstone(t *testing.T, db *gorm.DB, id, status string, deletedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, parse_status, updated_at, deleted_at) VALUES (?, ?, ?, ?)`,
		id, status, deletedAt, deletedAt,
	).Error)
}

// insertTombstoneChildren attaches one row to every table the purge must
// clean up, so a missing table in the transaction fails the test.
func insertTombstoneChildren(t *testing.T, db *gorm.DB, kid string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO chunks (id, knowledge_id, content, deleted_at) VALUES (?, ?, 'body', ?)`,
		kid+"-chunk-1", kid, time.Now(),
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO chunks (id, knowledge_id, content) VALUES (?, ?, 'body')`,
		kid+"-chunk-2", kid,
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO chunk_revisions (id, knowledge_id, chunk_id) VALUES (?, ?, ?)`,
		kid+"-rev-1", kid, kid+"-chunk-1",
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_tag_relations (knowledge_id, tag_id) VALUES (?, 'tag-1')`, kid,
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO embeddings (knowledge_id, chunk_id) VALUES (?, ?)`, kid, kid+"-chunk-1",
	).Error)
	insertSpan(t, db, kid, 1, "span-1", types.SpanStatusDone, time.Now())
	insertWikiPendingOp(t, db, "kb-1", kid)
}

func countRows(t *testing.T, db *gorm.DB, table, kid string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Table(table).Where("knowledge_id = ?", kid).Count(&n).Error)
	return n
}

func countKnowledge(t *testing.T, db *gorm.DB, kid string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Unscoped().Table("knowledges").Where("id = ?", kid).Count(&n).Error)
	return n
}

func TestTombstonePurge_DisabledByDefault(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	insertTombstone(t, db, "kid-1", types.ParseStatusFailed, time.Now().Add(-72*time.Hour))

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(1), countKnowledge(t, db, "kid-1"))
}

func TestTombstonePurge_RemovesExpiredDocumentAndChildren(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "1")
	t.Setenv(tombstoneDryRunEnv, "")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	insertTombstone(t, db, "kid-1", types.ParseStatusFailed, time.Now().Add(-48*time.Hour))
	insertTombstoneChildren(t, db, "kid-1")

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(0), countKnowledge(t, db, "kid-1"), "document row must be physically gone")
	for _, table := range []string{"chunks", "chunk_revisions", "embeddings", "knowledge_processing_spans", "knowledge_tag_relations"} {
		assert.Equal(t, int64(0), countRows(t, db, table, "kid-1"), "%s must lose its rows", table)
	}
	var ops int64
	require.NoError(t, db.Table("task_pending_ops").Where("dedup_key = ?", "kid-1").Count(&ops).Error)
	assert.Equal(t, int64(0), ops, "parked wiki op must be dropped")
}

func TestTombstonePurge_KeepsRowsWithinRetention(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "7")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	insertTombstone(t, db, "kid-1", types.ParseStatusFailed, time.Now().Add(-2*time.Hour))
	insertTombstoneChildren(t, db, "kid-1")

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(1), countKnowledge(t, db, "kid-1"))
	assert.Equal(t, int64(2), countRows(t, db, "chunks", "kid-1"))
}

func TestTombstonePurge_SkipsRowStillDeleting(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "1")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	// Old enough to purge, but its own delete task may still be running:
	// these are the rows that task is working on.
	insertTombstone(t, db, "kid-1", types.ParseStatusDeleting, time.Now().Add(-48*time.Hour))

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(1), countKnowledge(t, db, "kid-1"))
}

func TestTombstonePurge_LeavesLiveDocumentsAlone(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "1")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	insertKnowledge(t, db, "kid-live", types.ParseStatusCompleted, time.Now().Add(-72*time.Hour))
	insertTombstoneChildren(t, db, "kid-live")

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(1), countKnowledge(t, db, "kid-live"))
	assert.Equal(t, int64(2), countRows(t, db, "chunks", "kid-live"))
}

func TestTombstonePurge_DryRunDeletesNothing(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "1")
	t.Setenv(tombstoneDryRunEnv, "true")
	db := setupTombstoneDB(t)
	h := newHousekeepingSvcForTest(db)
	insertTombstone(t, db, "kid-1", types.ParseStatusFailed, time.Now().Add(-48*time.Hour))
	insertTombstoneChildren(t, db, "kid-1")

	h.purgeTombstones(context.Background())

	assert.Equal(t, int64(1), countKnowledge(t, db, "kid-1"))
	assert.Equal(t, int64(2), countRows(t, db, "chunks", "kid-1"))
}

func TestTombstoneRetention_FloorAndInvalidValues(t *testing.T) {
	t.Setenv(tombstoneRetentionEnv, "0")
	assert.Zero(t, tombstoneRetention(), "0 disables purging")

	t.Setenv(tombstoneRetentionEnv, "not-a-number")
	assert.Zero(t, tombstoneRetention())

	t.Setenv(tombstoneRetentionEnv, "-3")
	assert.Zero(t, tombstoneRetention())

	t.Setenv(tombstoneRetentionEnv, "1")
	assert.Equal(t, tombstoneMinRetention, tombstoneRetention())
}
