package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// The service is the last bound before an import starts, and it is the only
// one that holds for callers that never pass through the HTTP handler. The
// zero-value knowledgeService is enough: the refusal happens before any
// dependency is touched, so a regression that moved the check later would
// panic here rather than silently pass.
func TestUpsertFAQEntriesRefusesOverLimitBeforeTouchingDependencies(t *testing.T) {
	t.Setenv(secutils.EnvFAQImportMaxEntries, "3")
	svc := &knowledgeService{}

	entries := make([]types.FAQEntryPayload, 4)
	for i := range entries {
		entries[i] = types.FAQEntryPayload{StandardQuestion: "q"}
	}

	_, err := svc.UpsertFAQEntries(t.Context(), "kb-1", &types.FAQBatchUpsertPayload{
		Mode:    types.FAQBatchModeAppend,
		Entries: entries,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "单次最多导入 3 条")
}

// replace cannot be split into batches, so its refusal has to say so: the
// generic "submit in batches" advice would delete everything outside each
// batch.
func TestUpsertFAQEntriesReplaceRefusalExplainsWhyBatchingFails(t *testing.T) {
	t.Setenv(secutils.EnvFAQImportMaxEntries, "2")
	svc := &knowledgeService{}
	entries := []types.FAQEntryPayload{{StandardQuestion: "a"}, {StandardQuestion: "b"}, {StandardQuestion: "c"}}

	_, err := svc.UpsertFAQEntries(t.Context(), "kb-1", &types.FAQBatchUpsertPayload{
		Mode:    types.FAQBatchModeReplace,
		Entries: entries,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "replace 模式下拆小请求会把批外条目标记为待删除")
}

// At the limit the request proceeds past the check; it then fails on the
// missing repository rather than on the cap, which is what proves the cap let
// it through.
func TestUpsertFAQEntriesAcceptsExactlyTheLimit(t *testing.T) {
	t.Setenv(secutils.EnvFAQImportMaxEntries, "2")
	svc := &knowledgeService{}
	entries := []types.FAQEntryPayload{{StandardQuestion: "a"}, {StandardQuestion: "b"}}

	require.Panics(t, func() {
		_, _ = svc.UpsertFAQEntries(t.Context(), "kb-1", &types.FAQBatchUpsertPayload{
			Mode:    types.FAQBatchModeAppend,
			Entries: entries,
		})
	}, "an at-limit batch should reach the dependencies, not be refused by the cap")
}

// The HTTP handler caps these lists through their binding tags, but the service
// is the last bound before the work starts and it is the only one that holds
// for a caller that never passes through a handler. The numbers here are the
// documented ones, written out: an expectation built from the constant moves
// when the constant moves.
const documentedBatchCap = 20

// excludeIDsRefusal is the one refusal the exclude_ids guard may give. It is
// spelled out here rather than screened for banned words: every other thing a
// caller could be told -- split them, send them separately, reduce the count,
// keep using by_tag -- changes which rows the accompanying tag update touches.
func excludeIDsRefusal(cap, got int) string {
	return fmt.Sprintf(
		"单次最多排除 %d 条 FAQ，当前 %d 条，且排除列表不能拆批、不能截短。"+
			"请改用 by_id 直接更新需要的条目：每批最多 %d 条，且同时不带 by_tag 和 exclude_ids",
		cap, got, cap)
}

func overCapIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

func overCapByID(n int) map[int64]types.FAQEntryFieldsUpdate {
	m := make(map[int64]types.FAQEntryFieldsUpdate, n)
	for i := 0; i < n; i++ {
		m[int64(i+1)] = types.FAQEntryFieldsUpdate{}
	}
	return m
}

func overCapTags(n int) map[int64]*int64 {
	m := make(map[int64]*int64, n)
	for i := 0; i < n; i++ {
		m[int64(i+1)] = nil
	}
	return m
}

// faqBatchEntryOp is one service entry point and how to call it with a given
// number of ids. The first dependency each reaches is the KB lookup, so a
// zero-value service panics exactly when the cap let the call through — that
// distinguishes "accepted" from "refused" without standing up a database.
type faqBatchEntryOp struct {
	name  string
	run   func(t *testing.T, s *knowledgeService, n int) error
	label string
	// wantMessage, when set, is the exact refusal expected. It is used where
	// the wording itself carries the safety property: any other text might tell
	// the caller to do something destructive.
	wantMessage string
}

var faqBatchEntryOps = []faqBatchEntryOp{
	{
		name:  "fields by id",
		label: "单次最多更新",
		run: func(t *testing.T, s *knowledgeService, n int) error {
			return s.UpdateFAQEntryFieldsBatch(t.Context(), "kb-1", &types.FAQEntryFieldsBatchUpdate{ByID: overCapByID(n)})
		},
	},
	{
		name:  "exclude ids",
		label: "单次最多排除",
		// Splitting, shortening or re-running this list all change which rows
		// the by_tag update touches, so the refusal is asserted whole rather
		// than screened for phrases: a blocklist catches the wording it was
		// written for and waves through the next synonym.
		wantMessage: excludeIDsRefusal(documentedBatchCap, documentedBatchCap+1),
		run: func(t *testing.T, s *knowledgeService, n int) error {
			return s.UpdateFAQEntryFieldsBatch(t.Context(), "kb-1", &types.FAQEntryFieldsBatchUpdate{
				ByTag:      map[int64]types.FAQEntryFieldsUpdate{1: {}},
				ExcludeIDs: overCapIDs(n),
			})
		},
	},
	{
		name:  "tag batch",
		label: "单次最多更新",
		run: func(t *testing.T, s *knowledgeService, n int) error {
			return s.UpdateFAQEntryTagBatch(t.Context(), "kb-1", overCapTags(n))
		},
	},
	{
		name:  "delete",
		label: "单次最多删除",
		run: func(t *testing.T, s *knowledgeService, n int) error {
			return s.DeleteFAQEntries(t.Context(), "kb-1", overCapIDs(n))
		},
	},
}

// One over the cap is refused at the guard: an error naming the limit, and no
// dependency touched.
func TestBatchEntryOpsRefuseOverLimitBeforeTouchingDependencies(t *testing.T) {
	svc := &knowledgeService{}
	for _, op := range faqBatchEntryOps {
		t.Run(op.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				err := op.run(t, svc, documentedBatchCap+1)
				require.Error(t, err)
				require.Contains(t, err.Error(), op.label)
				if op.wantMessage != "" {
					// Exact, not a containment check: appending a helpful
					// sentence to a safe message is exactly how the unsafe
					// advice came back the last time.
					appErr, ok := err.(*werrors.AppError)
					require.True(t, ok, "expected an AppError, got %T", err)
					require.Equal(t, op.wantMessage, appErr.Message,
						"the refusal text is part of the contract here: only this wording is safe")
				}
			}, "an over-limit call must be refused before it reaches any dependency")
		})
	}
}

// Exactly at the cap reaches the dependencies. This is the half that catches an
// off-by-one guard: with `>` turned into `>=` the legal batch is refused here
// and nothing panics.
func TestBatchEntryOpsAcceptExactlyTheCap(t *testing.T) {
	svc := &knowledgeService{}
	for _, op := range faqBatchEntryOps {
		t.Run(op.name, func(t *testing.T) {
			require.Panics(t, func() {
				_ = op.run(t, svc, documentedBatchCap)
			}, "an at-limit batch should reach the dependencies, not be refused by the cap")
		})
	}
}

// The cap itself is 20, which is both the page size and what the docs promise.
func TestBatchEntryCapIsOnePage(t *testing.T) {
	if types.FAQBatchMaxEntriesPerRequest != documentedBatchCap {
		t.Fatalf("batch entry cap = %d, the documented value is %d",
			types.FAQBatchMaxEntriesPerRequest, documentedBatchCap)
	}
}

// Exclusions without a by_tag are inert: only the tag path reads them, so an
// over-limit list there cannot change a row. Refusing it would be a dead end --
// the caller's only remedy is by_id, and those batches carry the same list.
func TestExcludeIDsWithoutByTagAreAccepted(t *testing.T) {
	svc := &knowledgeService{}
	require.Panics(t, func() {
		_ = svc.UpdateFAQEntryFieldsBatch(t.Context(), "kb-1", &types.FAQEntryFieldsBatchUpdate{
			ByID:       overCapByID(2),
			ExcludeIDs: overCapIDs(documentedBatchCap + 5),
		})
	}, "an inert exclusion list must not block a by_id update")
}

// The same refused request reaches the HTTP layer and the service, and both
// have to word it identically. They build it from one helper now; this pins
// that, so an edit to one call site that changes the sentence is caught rather
// than surfacing as two different instructions for the same request.
func TestExclusionRefusalWordingIsSharedWithTheHTTPLayer(t *testing.T) {
	const got = types.FAQBatchMaxEntriesPerRequest + 1
	fields := map[int64]types.FAQEntryFieldsUpdate{7: {IsEnabled: boolPtr(false)}}
	req := &types.FAQEntryFieldsBatchUpdate{ByTag: fields, ExcludeIDs: overCapIDs(got)}

	err := types.ValidateFAQEntryFieldsBatch(req)
	require.Error(t, err)
	require.Equal(t, excludeIDsRefusal(types.FAQBatchMaxEntriesPerRequest, got), err.Error())
	require.Equal(t, secutils.FAQTooManyExclusionsMessage(types.FAQBatchMaxEntriesPerRequest, got), err.Error())
}
