package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
)

func resetFAQIngressBudget() {
	faqImportInflight.Store(0)
	faqImportInflightBytes.Store(0)
}

// The per-request cap bounds one body; this bounds how many bodies are held at
// once, which is what keeps a burst of maximum-size imports from exhausting a
// small host before any of them reaches the entry cap.
func TestFAQIngressBudgetRefusesBeyondTheInFlightLimit(t *testing.T) {
	resetFAQIngressBudget()
	t.Cleanup(resetFAQIngressBudget)
	t.Setenv(EnvFAQImportMaxInflight, "2")
	t.Setenv(EnvFAQImportMaxInflightBytes, "1000000")

	first, err := admitFAQImport(1000)
	require.NoError(t, err)
	second, err := admitFAQImport(1000)
	require.NoError(t, err)

	_, err = admitFAQImport(1000)
	require.Error(t, err)
	_, ok := err.(*apperrors.AppError)
	require.True(t, ok, "a refused import must surface as an API error, not a generic failure")

	first.release()
	third, err := admitFAQImport(1000)
	require.NoError(t, err, "releasing a slot has to make room for the next request")
	second.release()
	third.release()
	require.Zero(t, faqImportInflight.Load())
	require.Zero(t, faqImportInflightBytes.Load())
}

// Two requests that are each individually fine can still exceed the byte
// budget together; the sum is what matters.
func TestFAQIngressBudgetRefusesBeyondTheByteLimit(t *testing.T) {
	resetFAQIngressBudget()
	t.Cleanup(resetFAQIngressBudget)
	t.Setenv(EnvFAQImportMaxInflight, "100")
	t.Setenv(EnvFAQImportMaxInflightBytes, "2500")

	a, err := admitFAQImport(1000)
	require.NoError(t, err)
	b, err := admitFAQImport(1000)
	require.NoError(t, err)
	_, err = admitFAQImport(1000)
	require.Error(t, err, "the third request would push the held bytes past the budget")
	a.release()
	b.release()
}

// A release must be idempotent: the handler defers it and may also release on
// an early rejection path, and a double release would corrupt the counters.
func TestFAQIngressBudgetReleaseIsIdempotent(t *testing.T) {
	resetFAQIngressBudget()
	t.Cleanup(resetFAQIngressBudget)
	t.Setenv(EnvFAQImportMaxInflight, "10")
	t.Setenv(EnvFAQImportMaxInflightBytes, "100000")

	a, err := admitFAQImport(500)
	require.NoError(t, err)
	a.release()
	a.release()
	require.Zero(t, faqImportInflight.Load())
	require.Zero(t, faqImportInflightBytes.Load())
}

// The whole handler has to refuse, not merely log: an import body that is never
// read costs nothing, which is the point of taking the slot before the read.
func TestUpsertFAQsRefusesWhenTheBudgetIsFull(t *testing.T) {
	resetFAQIngressBudget()
	t.Cleanup(resetFAQIngressBudget)
	t.Setenv(EnvFAQImportMaxInflight, "1")
	t.Setenv(EnvFAQImportMaxInflightBytes, "100000000")

	held, err := admitFAQImport(1000)
	require.NoError(t, err)
	defer held.release()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/knowledge-bases/kb-1/faq/entries", strings.NewReader(faqEntriesBody(1)))
	c.Params = gin.Params{{Key: "id", Value: "kb-1"}}
	svc := &fakeFAQUpsertService{}
	h := &FAQHandler{knowledgeService: svc}
	h.UpsertEntries(c)

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, appErr.HTTPCode)
	require.Zero(t, svc.calls, "the refused request must not reach the service")
}
