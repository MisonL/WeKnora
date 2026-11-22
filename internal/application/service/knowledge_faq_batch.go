package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"gorm.io/gorm"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func sortedFAQIDs[V any](values map[int64]V) []int64 {
	ids := make([]int64, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// loadFAQWriteChunks validates the entire selection before the first mutation.
// Copies keep planning from mutating repository-owned snapshots on rejection.
func (s *knowledgeService) loadFAQWriteChunks(
	ctx context.Context,
	kb *types.KnowledgeBase,
	ids []int64,
) (map[int64]*types.Chunk, error) {
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, apperrors.NewBadRequestError("FAQ 条目 ID 必须为正整数")
		}
		wanted[id] = true
	}
	result := make(map[int64]*types.Chunk, len(wanted))
	if len(wanted) == 0 {
		return result, nil
	}
	chunks, err := s.chunkRepo.ListChunksBySeqID(ctx, kb.TenantID, sortedFAQIDs(wanted))
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunks {
		if chunk == nil || !wanted[chunk.SeqID] {
			continue
		}
		if chunk.TenantID != kb.TenantID || chunk.KnowledgeBaseID != kb.ID || chunk.ChunkType != types.ChunkTypeFAQ {
			return nil, apperrors.NewForbiddenError("FAQ 条目不属于当前知识库")
		}
		snapshot := *chunk
		result[chunk.SeqID] = &snapshot
	}
	if len(result) != len(wanted) {
		return nil, apperrors.NewNotFoundError("FAQ 条目不存在")
	}
	return result, nil
}

type faqFieldPlan struct {
	chunks     map[int64]*types.Chunk
	chunksByID map[string]*types.Chunk
	tags       map[int64]*types.KnowledgeTag
	excludeIDs []string
}

func (s *knowledgeService) planFAQFields(
	ctx context.Context,
	kb *types.KnowledgeBase,
	req *types.FAQEntryFieldsBatchUpdate,
) (*faqFieldPlan, error) {
	// Exclusions are only meaningful to a by_tag update: plan.excludeIDs is
	// handed to the repository call that path makes, and nothing else reads it.
	// Loading and validating them unconditionally would let a request that
	// cannot use them cost a lookup per id -- and, with the cap relaxed for
	// exactly that reason, make an unbounded exclusion list reachable.
	exclusions := req.ExcludeIDs
	if len(req.ByTag) == 0 {
		exclusions = nil
	}
	ids := append(sortedFAQIDs(req.ByID), exclusions...)
	chunks, err := s.loadFAQWriteChunks(ctx, kb, ids)
	if err != nil {
		return nil, err
	}
	plan := &faqFieldPlan{
		chunks:     chunks,
		chunksByID: make(map[string]*types.Chunk),
		tags:       make(map[int64]*types.KnowledgeTag),
	}
	for _, chunk := range chunks {
		plan.chunksByID[chunk.ID] = chunk
	}
	for _, id := range exclusions {
		plan.excludeIDs = append(plan.excludeIDs, chunks[id].ID)
	}
	tagIDs := make(map[int64]bool)
	for id := range req.ByTag {
		if id <= 0 {
			return nil, apperrors.NewBadRequestError("标签 ID 必须为正整数")
		}
		tagIDs[id] = true
	}
	for _, updates := range []map[int64]types.FAQEntryFieldsUpdate{req.ByTag, req.ByID} {
		for _, update := range updates {
			if update.TagID != nil && *update.TagID > 0 {
				tagIDs[*update.TagID] = true
			}
		}
	}
	if len(tagIDs) > 0 {
		tags, err := s.tagRepo.GetBySeqIDs(ctx, kb.TenantID, sortedFAQIDs(tagIDs))
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			if tag == nil || !tagIDs[tag.SeqID] {
				continue
			}
			if err := validateFAQTagScope(tag, kb.TenantID, kb.ID); err != nil {
				return nil, err
			}
			plan.tags[tag.SeqID] = tag
		}
		for _, id := range sortedFAQIDs(tagIDs) {
			if plan.tags[id] == nil {
				return nil, apperrors.NewNotFoundError(fmt.Sprintf("标签 %d 不存在", id))
			}
		}
	}
	return plan, nil
}

func validateFAQTagScope(tag *types.KnowledgeTag, tenantID uint64, kbID string) error {
	if tag == nil {
		return apperrors.NewNotFoundError("标签不存在")
	}
	if tag.TenantID != tenantID || tag.KnowledgeBaseID != kbID {
		return apperrors.NewForbiddenError("标签不属于当前知识库")
	}
	return nil
}

func (s *knowledgeService) validateFAQImportTags(
	ctx context.Context,
	kb *types.KnowledgeBase,
	entries []types.FAQEntryPayload,
) error {
	// validateFAQImportTags
	//
	// Import still reports per-entry content validation failures. Referenced
	// resources must all be in scope before admission or execution, however.
	//
	// A tag id names a tag in one knowledge base. An export of KB A imported
	// into KB B carries A's ids, which are out of scope in B and would refuse
	// the whole import -- even though every entry also names its tag. Those ids
	// are cleared here and resolved by name instead; an id with no name is left
	// alone so the planner reports it.
	normaliseFAQImportTagIDs(ctx, s.tagRepo, kb.TenantID, kb.ID, entries)

	req := &types.FAQEntryFieldsBatchUpdate{ByTag: make(map[int64]types.FAQEntryFieldsUpdate)}
	for _, entry := range entries {
		if entry.TagID != 0 {
			req.ByTag[entry.TagID] = types.FAQEntryFieldsUpdate{}
		}
	}
	_, err := s.planFAQFields(ctx, kb, req)
	return err
}

// normaliseFAQImportTagIDs clears tag ids that do not resolve to a tag of the
// target knowledge base when the entry names its tag as well. Lookups are
// cached per id: a batch references a handful of tags, not one per entry.
func normaliseFAQImportTagIDs(
	ctx context.Context,
	tagRepo interfaces.KnowledgeTagRepository,
	tenantID uint64,
	kbID string,
	entries []types.FAQEntryPayload,
) {
	if tagRepo == nil || kbID == "" {
		return
	}
	// unresolvable[id] is true when the id names no tag of this KB. A lookup
	// that fails for another reason (a database error) must not clear the id:
	// clearing it would silently turn "reuse this tag" into "create a tag with
	// this name". Left in place, the planner's own lookup reports the error.
	unresolvable := make(map[int64]bool)
	checked := make(map[int64]bool)
	for i := range entries {
		id := entries[i].TagID
		if id == 0 {
			continue
		}
		if !checked[id] {
			checked[id] = true
			unresolvable[id] = false
			tag, err := tagRepo.GetBySeqID(ctx, tenantID, id)
			switch {
			case err == nil && tag != nil && tag.TenantID == tenantID && tag.KnowledgeBaseID == kbID:
				// In scope: keep it.
			case err == nil && tag != nil, errors.Is(err, gorm.ErrRecordNotFound), err == nil && tag == nil:
				// It exists but belongs elsewhere (another KB or tenant), or
				// nothing has this id; a name fallback is safe.
				unresolvable[id] = true
			default:
				logger.Warnf(ctx, "FAQ import: could not look up tag id %d: %v; keeping it", id, err)
			}
		}
		if unresolvable[id] && entries[i].TagName != "" {
			logger.Infof(ctx, "FAQ import: tag id %d does not belong to KB %s; using tag name %q",
				id, kbID, entries[i].TagName)
			entries[i].TagID = 0
		}
	}
}
