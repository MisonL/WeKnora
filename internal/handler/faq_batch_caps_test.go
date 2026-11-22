package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// faqBatchCapService records whether the handler let a request through.
type faqBatchCapService struct {
	interfaces.KnowledgeService
	deleted    [][]int64
	tagUpdates []map[int64]*int64
	fields     []*types.FAQEntryFieldsBatchUpdate
}

func (s *faqBatchCapService) DeleteFAQEntries(_ context.Context, _ string, ids []int64) error {
	s.deleted = append(s.deleted, ids)
	return nil
}

func (s *faqBatchCapService) UpdateFAQEntryTagBatch(_ context.Context, _ string, updates map[int64]*int64) error {
	s.tagUpdates = append(s.tagUpdates, updates)
	return nil
}

func (s *faqBatchCapService) UpdateFAQEntryFieldsBatch(
	_ context.Context, _ string, req *types.FAQEntryFieldsBatchUpdate,
) error {
	s.fields = append(s.fields, req)
	return nil
}

func faqCapProbe(
	t *testing.T, method, body string, svc *faqBatchCapService, invoke func(*FAQHandler, *gin.Context),
) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/knowledge-bases/kb-1/faq/entries", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "kb-1"}}
	invoke(&FAQHandler{knowledgeService: svc}, c)
	return c
}

// faqIDListBody builds a {"ids":[...]} payload with n ids.
func faqIDListBody(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d", i+1)
	}
	return `{"ids":[` + strings.Join(ids, ",") + `]}`
}

// faqTagUpdatesBody builds a {"updates":{...}} payload with n keys.
func faqTagUpdatesBody(n int) string {
	pairs := make([]string, n)
	for i := range pairs {
		pairs[i] = fmt.Sprintf(`"%d":%d`, i+1, i+1)
	}
	return `{"updates":{` + strings.Join(pairs, ",") + `}}`
}

// The batch delete cap is enforced, not merely advertised. Each id costs a
// chunk delete and a vector delete; the UI can only select one 20-row page, so
// a larger list is a caller bypassing the product rather than a use case.
func TestDeleteFAQEntriesRejectsMoreThanOnePage(t *testing.T) {
	svc := &faqBatchCapService{}
	c := faqCapProbe(t, http.MethodDelete, faqIDListBody(21), svc,
		func(h *FAQHandler, c *gin.Context) { h.DeleteEntries(c) })

	require.NotEmpty(t, c.Errors)
	// Name the reason: any other bind failure would also produce an error, so
	// asserting only "there is one" would stay green if the cap were removed.
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	details, _ := appErr.Details.(string)
	require.Contains(t, details, "max", "the refusal must come from the size cap, not another validation")
	require.Empty(t, svc.deleted, "an over-limit delete must not reach the service")
}

func TestDeleteFAQEntriesAcceptsOnePage(t *testing.T) {
	svc := &faqBatchCapService{}
	c := faqCapProbe(t, http.MethodDelete, faqIDListBody(20), svc,
		func(h *FAQHandler, c *gin.Context) { h.DeleteEntries(c) })

	require.Empty(t, c.Errors)
	require.Len(t, svc.deleted, 1)
	require.Len(t, svc.deleted[0], 20)
}

func TestUpdateFAQEntryTagsRejectsMoreThanOnePage(t *testing.T) {
	svc := &faqBatchCapService{}
	c := faqCapProbe(t, http.MethodPut, faqTagUpdatesBody(21), svc,
		func(h *FAQHandler, c *gin.Context) { h.UpdateEntryTagBatch(c) })

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	details, _ := appErr.Details.(string)
	require.Contains(t, details, "max", "the refusal must come from the size cap, not another validation")
	require.Empty(t, svc.tagUpdates)
}

// Exactly at the cap is legal: the page size and the cap have to agree.
func TestUpdateFAQEntryTagsAcceptsOnePage(t *testing.T) {
	svc := &faqBatchCapService{}
	c := faqCapProbe(t, http.MethodPut, faqTagUpdatesBody(20), svc,
		func(h *FAQHandler, c *gin.Context) { h.UpdateEntryTagBatch(c) })

	require.Empty(t, c.Errors)
	require.Len(t, svc.tagUpdates, 1)
	require.Len(t, svc.tagUpdates[0], 20)
}

// ByTag is deliberately unbounded — updating a tag is defined to touch every
// entry that carries it — but the explicit ByID map is a list of entries and
// gets the same page-sized cap as the other list endpoints.
func TestUpdateFAQEntryFieldsCapsByIDNotByTag(t *testing.T) {
	svc := &faqBatchCapService{}

	tooMany := `{"by_id":{` + strings.Join(func() []string {
		pairs := make([]string, 21)
		for i := range pairs {
			pairs[i] = fmt.Sprintf(`"%d":{"is_enabled":true}`, i+1)
		}
		return pairs
	}(), ",") + `}}`
	c := faqCapProbe(t, http.MethodPut, tooMany, svc,
		func(h *FAQHandler, c *gin.Context) { h.UpdateEntryFieldsBatch(c) })
	require.NotEmpty(t, c.Errors, "21 by_id entries should be refused")
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	details, _ := appErr.Details.(string)
	require.Contains(t, details, "max", "the refusal must come from the size cap, not another validation")
	require.Empty(t, svc.fields)

	// A single tag selector stays legal regardless of how many entries it will
	// touch.
	byTag := `{"by_tag":{"5":{"is_enabled":false}}}`
	c = faqCapProbe(t, http.MethodPut, byTag, svc,
		func(h *FAQHandler, c *gin.Context) { h.UpdateEntryFieldsBatch(c) })
	require.Empty(t, c.Errors)
	require.Len(t, svc.fields, 1)
}

// faqIDArray renders the bare JSON array of n ids.
func faqIDArray(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d", i+1)
	}
	return "[" + strings.Join(ids, ",") + "]"
}

// exclude_ids is a caller-supplied list, so it gets the same page-sized cap as
// the other list endpoints: a bypassed client could otherwise make one tag
// update skip an arbitrary number of entries, each costing a per-entry update.
func TestUpdateFAQEntryFieldsCapsExcludeIDs(t *testing.T) {
	svc := &faqBatchCapService{}
	invoke := func(h *FAQHandler, c *gin.Context) { h.UpdateEntryFieldsBatch(c) }

	// A by_tag update is the only thing that reads exclusions, and every row it
	// fails to exclude gets changed -- so this is where the cap has to hold.
	c := faqCapProbe(t, http.MethodPut,
		`{"by_tag":{"5":{"is_enabled":false}},"exclude_ids":`+faqIDArray(21)+`}`, svc, invoke)
	require.NotEmpty(t, c.Errors, "21 exclude_ids with a by_tag should be refused")
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, fmt.Sprint(appErr.Details), "exclude_ids",
		"the refusal must name the list, not some other field")
	require.Empty(t, svc.fields)

	// Without a by_tag the list is inert: it drives no lookup and changes no
	// row, so bounding it would refuse a request that cannot do anything.
	c = faqCapProbe(t, http.MethodPut,
		`{"by_id":{"1":{"is_enabled":false}},"exclude_ids":`+faqIDArray(21)+`}`, svc, invoke)
	require.Empty(t, c.Errors, "an inert exclusion list must not block a by_id update")
	require.Len(t, svc.fields, 1)

	// Exactly at the cap must still go through, so the cap is not off by one
	// against what the client sends.
	svc = &faqBatchCapService{}
	c = faqCapProbe(t, http.MethodPut,
		`{"by_tag":{"5":{"is_enabled":false}},"exclude_ids":`+faqIDArray(20)+`}`, svc, invoke)
	require.Empty(t, c.Errors)
	require.Len(t, svc.fields, 1)
	require.Len(t, svc.fields[0].ExcludeIDs, 20)
}
