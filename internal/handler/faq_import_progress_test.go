package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

type faqProgressService struct {
	interfaces.KnowledgeService
	progress *types.FAQImportProgress
	err      error
}

func (s *faqProgressService) GetFAQImportProgress(context.Context, string) (*types.FAQImportProgress, error) {
	return s.progress, s.err
}

// The progress record's instance id and enqueue time are the server's fencing
// identity for a run; clients address a task by task_id and never use them.
// Handing them out exposes internal run identity for no benefit.
func TestGetImportProgressDoesNotExposeTheRunIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	taskID := utils.GenerateTaskID("faq_import", 7, "kb1")
	svc := &faqProgressService{progress: &types.FAQImportProgress{
		TaskID:            taskID,
		KBID:              "kb1",
		Status:            types.FAQImportStatusProcessing,
		InstanceID:        "5b0d0f43-1111-2222-3333-444455556666",
		EnqueuedAt:        1_700_000_000,
		ValidEntryIndices: []int{0, 1},
		FailedEntries:     []types.FAQFailedEntry{{Index: 0, Reason: "x"}},
	}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/faq/import/"+taskID, nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(7)))
	c.Params = gin.Params{{Key: "task_id", Value: taskID}}

	(&FAQHandler{knowledgeService: svc}).GetImportProgress(c)
	require.Empty(t, c.Errors)

	var body struct {
		Success bool                       `json:"success"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.JSONEq(t, `"`+taskID+`"`, string(body.Data["task_id"]))
	_, hasInstance := body.Data["instance_id"]
	_, hasEnqueued := body.Data["enqueued_at"]
	require.False(t, hasInstance, "instance_id is internal run identity")
	require.False(t, hasEnqueued, "enqueued_at is internal run identity")
	// The rest of the record still reaches the client.
	require.Equal(t, `"processing"`, string(body.Data["status"]))
	require.JSONEq(t, `[0,1]`, string(body.Data["valid_entry_indices"]))
}
