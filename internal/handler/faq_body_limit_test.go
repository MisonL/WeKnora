package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"

	"github.com/Tencent/WeKnora/internal/utils"
)

// upsertFAQProbe drives the batch-upsert handler with one body. The service is
// only reached when binding succeeded, so a nil service doubles as proof that
// the payload never got past the read.
func upsertFAQProbe(
	t *testing.T, body string, svc *fakeFAQUpsertService,
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/knowledge-bases/kb-1/faq/entries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "kb-1"}}

	h := &FAQHandler{knowledgeService: svc}
	h.UpsertEntries(c)
	return c, w
}

type fakeFAQUpsertService struct {
	interfaces.KnowledgeService
	calls int
}

func (s *fakeFAQUpsertService) UpsertFAQEntries(
	_ context.Context, _ string, _ *types.FAQBatchUpsertPayload,
) (string, error) {
	s.calls++
	return "task-1", nil
}

// The batch upsert payload's entries array has no count limit, and validation
// only runs after ShouldBindJSON has materialized the whole array, so without a
// reader cap one request can make the process hold an arbitrarily large body.
func TestUpsertFAQsRejectsAnOversizedBody(t *testing.T) {
	oversize := int(faqJSONMaxBytes()) + 1
	body := `{"entries":[{"standard_question":"` + strings.Repeat("a", oversize) + `"}]}`

	c, _ := upsertFAQProbe(t, body, &fakeFAQUpsertService{})

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*errors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Equal(t, errors.ErrPayloadTooLarge, appErr.Code,
		"an oversized body is 413: shrinking it is the fix, and the same body retried fails the same way")
	require.Equal(t, http.StatusRequestEntityTooLarge, appErr.HTTPCode)
	// The message names the budget. This body carries no mode, so the advice
	// must not promise that splitting is safe: only append can be split, and a
	// replace re-sent in pieces would delete whatever fell outside each piece.
	require.Contains(t, appErr.Message, strconv.FormatInt(faqJSONMaxBytes(), 10))
	require.Contains(t, appErr.Message, "只有 append 模式可以拆批提交")
}

// The byte-budget message must never assume append. A body the size cap cut
// off was not read to the end, so even a mode field it did contain is only a
// prefix -- a later `"mode":"replace"` may sit past the cut. The wording
// therefore always carries the conditional form and names the knob.
func TestUpsertFAQsOversizedBodyWarnsReplaceCallers(t *testing.T) {
	oversize := int(faqJSONMaxBytes()) + 1
	body := `{"mode":"replace","entries":[{"standard_question":"` + strings.Repeat("a", oversize) + `"}]}`

	c, _ := upsertFAQProbe(t, body, &fakeFAQUpsertService{})

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*errors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.NotContains(t, appErr.Message, "请拆成几批提交")
	require.Contains(t, appErr.Message, "replace")
}

// A repeated mode is legal JSON, and the decoder keeps the LAST one. If the
// second value sits past the byte budget's cut, the request's real mode is
// unknown -- reporting the pre-truncation value would advise splitting a
// replace. Both orders are checked, because either one can be the one cut off.
func TestUpsertFAQsTruncatedDuplicateModeIsTreatedAsUnknown(t *testing.T) {
	pad := strings.Repeat("x", 128)
	for name, body := range map[string]string{
		"append then unseen replace": `{"mode":"append","padding":"` + pad + `","mode":"replace","entries":[{"standard_question":"q"}]}`,
		"replace then unseen append": `{"mode":"replace","padding":"` + pad + `","mode":"append","entries":[{"standard_question":"q"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(utils.EnvFAQImportMaxJSONBytes, "96")
			c, _ := upsertFAQProbe(t, body, &fakeFAQUpsertService{})
			require.NotEmpty(t, c.Errors, "the body is over the byte budget")
			appErr, ok := c.Errors[0].Err.(*errors.AppError)
			require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
			// Only the append-specific advice may say "split it up", and this
			// body does not establish append.
			require.NotContains(t, appErr.Message, "请拆成几批提交",
				"a truncated body does not establish its mode, so splitting must not be advised")
			require.Contains(t, appErr.Message, "只有 append 模式可以拆批提交")
		})
	}
}

// A truncated body never read the whole document, and JSON puts no constraint
// on field order, so the mode may sit after the entries the size cap cut off.
// Advising "split it into batches" then is only safe if the body is known to be
// an append: a replace re-sent in pieces deletes everything outside each piece.
func TestUpsertFAQsOversizedBodyDoesNotAssumeAppend(t *testing.T) {
	oversize := int(faqJSONMaxBytes()) + 1
	for name, body := range map[string]string{
		"mode after the entries": `{"entries":[{"standard_question":"` + strings.Repeat("a", oversize) + `"}],"mode":"replace"}`,
		"mode before, replace":   `{"mode":"replace","entries":[{"standard_question":"` + strings.Repeat("a", oversize) + `"}]}`,
		"no mode at all":         `{"entries":[{"standard_question":"` + strings.Repeat("a", oversize) + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := upsertFAQProbe(t, body, &fakeFAQUpsertService{})
			require.NotEmpty(t, c.Errors)
			appErr, ok := c.Errors[0].Err.(*errors.AppError)
			require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
			require.NotContains(t, appErr.Message, "请拆成几批提交",
				"nothing about this body proves it is an append, so the bare advice is wrong")
		})
	}
}

// The same applies to the over-limit path: a duplicate mode is legal JSON and
// the decoder keeps the last one, so a first-match scan would word the refusal
// for a mode the request does not actually run in.
func TestUpsertFAQsOverLimitUsesTheLastMode(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "2")
	body := `{"mode":"append","entries":[{"standard_question":"a"},{"standard_question":"b"},{"standard_question":"c"}],"mode":"replace"}`

	c, _ := upsertFAQProbe(t, body, &fakeFAQUpsertService{})

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*errors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Message, "拆小请求会把批外条目标记为待删除",
		"the last mode wins, so this is a replace request")
}

// Every FAQ JSON route binds its body, and AddSimilarQuestions used to read an
// unbounded one: the reader had no cap at all, so a caller could make the
// process hold whatever it sent before any validation ran.
func TestAddSimilarQuestionsRejectsAnOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"similar_questions":["` + strings.Repeat("a", int(faqJSONMaxBytes())+1) + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/knowledge-bases/kb-1/faq/entries/1/similar-questions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "kb-1"}, {Key: "entry_id", Value: "1"}}

	h := &FAQHandler{}
	h.AddSimilarQuestions(c)

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*errors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Equal(t, errors.ErrPayloadTooLarge, appErr.Code)
	require.Equal(t, http.StatusRequestEntityTooLarge, appErr.HTTPCode)
}

// The plain binding path stays 400 for a malformed body: only an oversized one
// is distinguishable and actionable as 413.
func TestFAQJSONBindingKeepsMalformedAsBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"similar_questions":`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	var dst addSimilarQuestionsRequest
	require.False(t, bindFAQJSON(c, &dst))
	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*errors.AppError)
	require.True(t, ok)
	require.Equal(t, errors.ErrBadRequest, appErr.Code)
}
