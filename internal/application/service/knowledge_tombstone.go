package service

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Tombstone purge.
//
// Deleting a document is a soft delete: the document row keeps its deleted_at
// and its chunks keep theirs, but nothing ever removes either. The sweeps in
// knowledge_housekeeping.go only settle their status so they stop being
// scanned — every bulk delete therefore leaves a permanent set of rows and
// chunk tombstones behind (measured on the QDAMA deployment: 198 documents,
// ~36.7k chunk rows, ~6.5k processing spans, 0 of them reachable by any API or
// retrieval query).
//
// Recovering a soft-deleted document has never been possible: the delete flow
// removes the source object from file storage and the vectors from the index
// before it writes deleted_at. The tombstones are storage hygiene, not a
// recovery path, so purging them is safe once they are past retention.
//
// Purging is irreversible, so it is opt-in: nothing runs unless
// WEKNORA_TOMBSTONE_RETENTION_DAYS is a positive number of days. Dry-run mode
// (WEKNORA_TOMBSTONE_DRY_RUN=true) reports what would be removed without
// touching a row — use it to size the first cleanup before arming the sweep.

const (
	// tombstonePurgeBatch bounds one sweep. The first cleanup after this
	// feature ships can face tens of thousands of chunk tombstones, so the
	// backlog is drained a couple hundred documents per sweep instead of one
	// long transaction.
	tombstonePurgeBatch = 200
	// tombstoneMinRetention is the smallest retention we accept. Anything
	// shorter would race a user who deleted the wrong document and is
	// watching the queue drain.
	tombstoneMinRetention = 24 * time.Hour

	tombstoneRetentionEnv = "WEKNORA_TOMBSTONE_RETENTION_DAYS"
	tombstoneDryRunEnv    = "WEKNORA_TOMBSTONE_DRY_RUN"
)

// vectorRow maps the PostgreSQL retriever's `embeddings` table. Its model is
// unexported inside the retriever package, and the other vector backends
// (OpenSearch, Qdrant, Milvus, ...) keep vectors outside this database, so the
// table-exists guard skips the delete on every deployment that does not have
// it.
type vectorRow struct {
	ID uint `gorm:"primaryKey"`
}

func (vectorRow) TableName() string { return "embeddings" }

// tombstoneRetention returns how long soft-deleted rows are kept, or 0 when
// purging is disabled.
func tombstoneRetention() time.Duration {
	v := strings.TrimSpace(os.Getenv(tombstoneRetentionEnv))
	if v == "" {
		return 0
	}
	days, err := strconv.Atoi(v)
	if err != nil || days <= 0 {
		return 0
	}
	retention := time.Duration(days) * 24 * time.Hour
	if retention < tombstoneMinRetention {
		return tombstoneMinRetention
	}
	return retention
}

func tombstoneDryRun() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(tombstoneDryRunEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// purgeTombstones permanently removes soft-deleted documents — together with
// their chunks, vectors, revisions, processing spans, queue entries and tag
// links — once they are older than the retention. A document still in
// "deleting" is skipped: its own delete task has not finished, and these are
// the rows it is working on.
//
// Each document is purged in its own transaction so a failure drains the rest
// of the batch instead of stalling it, and so an interrupted sweep never
// leaves a document without chunks but with its row still present.
func (h *HousekeepingService) purgeTombstones(ctx context.Context) {
	retention := tombstoneRetention()
	if retention <= 0 {
		return
	}
	cutoff := time.Now().Add(-retention)
	dryRun := tombstoneDryRun()

	var ids []string
	if err := h.db.WithContext(ctx).Unscoped().
		Model(&types.Knowledge{}).
		Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
		Where("parse_status <> ?", types.ParseStatusDeleting).
		Order("deleted_at ASC").
		Limit(tombstonePurgeBatch).
		Pluck("id", &ids).Error; err != nil {
		logger.Warnf(ctx, "[Tombstone] candidate query failed: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}

	if dryRun {
		var chunks, spans, pending int64
		_ = h.db.WithContext(ctx).Unscoped().Model(&types.Chunk{}).
			Where("knowledge_id IN ?", ids).Count(&chunks).Error
		_ = h.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
			Where("knowledge_id IN ?", ids).Count(&spans).Error
		_ = h.db.WithContext(ctx).Model(&types.TaskPendingOp{}).
			Where("dedup_key IN ?", ids).Count(&pending).Error
		logger.Infof(ctx,
			"[Tombstone] dry run: would purge %d document(s) deleted before %s (%d chunk(s), %d span(s), %d queued op(s))",
			len(ids), cutoff.Format(time.RFC3339), chunks, spans, pending)
		return
	}

	var docs, chunks, spans, ops int64
	hasVectors := h.db.Migrator().HasTable(&vectorRow{})
	for _, id := range ids {
		var docChunks, docSpans, docOps int64
		err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// Children first, then the row. Every table below keys on
			// knowledge_id and has no foreign key back to knowledges, so
			// deleting the row first would orphan them silently.
			res := tx.Unscoped().Where("knowledge_id = ?", id).Delete(&types.Chunk{})
			if res.Error != nil {
				return res.Error
			}
			docChunks = res.RowsAffected

			if res := tx.Where("knowledge_id = ?", id).Delete(&types.ChunkRevision{}); res.Error != nil {
				return res.Error
			}
			if hasVectors {
				if res := tx.Where("knowledge_id = ?", id).Delete(&vectorRow{}); res.Error != nil {
					return res.Error
				}
			}
			res = tx.Where("knowledge_id = ?", id).Delete(&types.KnowledgeProcessingSpan{})
			if res.Error != nil {
				return res.Error
			}
			docSpans = res.RowsAffected
			if res := tx.Where("knowledge_id = ?", id).Delete(&types.KnowledgeTagRelation{}); res.Error != nil {
				return res.Error
			}
			// The Wiki ingest queue keys parked ops by dedup_key =
			// knowledge ID; leaving them behind would keep the queue
			// non-empty for a document that will never arrive.
			res = tx.Where("dedup_key = ?", id).Delete(&types.TaskPendingOp{})
			if res.Error != nil {
				return res.Error
			}
			docOps = res.RowsAffected

			return tx.Unscoped().Where("id = ?", id).Delete(&types.Knowledge{}).Error
		})
		if err != nil {
			logger.Warnf(ctx, "[Tombstone] purge failed for knowledge %s: %v (will retry next sweep)", id, err)
			continue
		}
		docs++
		chunks += docChunks
		spans += docSpans
		ops += docOps
	}
	if docs > 0 {
		logger.Infof(ctx,
			"[Tombstone] purged %d document(s) deleted before %s — %d chunk(s), %d span(s), %d queued op(s)",
			docs, cutoff.Format(time.RFC3339), chunks, spans, ops)
	}
}
