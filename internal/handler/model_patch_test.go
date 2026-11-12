package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubPatchModelService struct {
	interfaces.ModelService
	model   *types.Model
	updated *types.Model
}

func (s *stubPatchModelService) GetModelByID(context.Context, string) (*types.Model, error) {
	copied := *s.model
	return &copied, nil
}

func (s *stubPatchModelService) UpdateModel(_ context.Context, model *types.Model) error {
	s.updated = model
	return nil
}

func patchModel(t *testing.T, stub *stubPatchModelService, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.ErrorHandler())
	router.PATCH("/models/:id", NewModelHandler(stub).PatchModel)
	req := httptest.NewRequest(http.MethodPatch, "/models/m1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPatchModelChangesOnlyTheDisplayFields(t *testing.T) {
	stub := &stubPatchModelService{model: &types.Model{
		ID: "m1", Name: "gpt-4o", DisplayName: "old", Description: "keep",
		Type: types.ModelTypeKnowledgeQA, Source: types.ModelSourceRemote,
		Parameters: types.ModelParameters{BaseURL: "https://api.example.com/v1", APIKey: "sk-live"},
	}}
	rec := patchModel(t, stub, `{"display_name":"  Production GPT  "}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, stub.updated)
	assert.Equal(t, "Production GPT", stub.updated.DisplayName)
	assert.Equal(t, "keep", stub.updated.Description)
	assert.Equal(t, "https://api.example.com/v1", stub.updated.Parameters.BaseURL)
	assert.Equal(t, "sk-live", stub.updated.Parameters.APIKey, "the stored key is saved back unchanged")
	assert.NotContains(t, rec.Body.String(), "sk-live")

	rec = patchModel(t, stub, `{"description":""}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "old", stub.updated.DisplayName)
	assert.Empty(t, stub.updated.Description)
}

func TestPatchModelNeedsAField(t *testing.T) {
	stub := &stubPatchModelService{model: &types.Model{ID: "m1"}}
	rec := patchModel(t, stub, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, stub.updated)
}

// A row whose API key could not be decrypted is loaded with an empty APIKey
// plus the original ciphertext retained by Scan. A display-only PATCH must
// hand that ciphertext back to the repository, so Select("*") cannot blank
// the column.
func TestPatchModelPreservesUndecryptableKeyCiphertext(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", "0123456789abcdef0123456789abcdef")
	stored, err := (&types.ModelParameters{APIKey: "sk-rotated", AppSecret: "sec-rotated"}).Value()
	require.NoError(t, err)

	t.Setenv("SYSTEM_AES_KEY", "fedcba9876543210fedcba9876543210")
	var loaded types.ModelParameters
	require.NoError(t, loaded.Scan(stored.([]byte)))
	require.Empty(t, loaded.APIKey)

	stub := &stubPatchModelService{model: &types.Model{
		ID: "m1", Name: "gpt-4o", DisplayName: "old",
		Type: types.ModelTypeKnowledgeQA, Source: types.ModelSourceRemote,
		Parameters: loaded,
	}}
	rec := patchModel(t, stub, `{"display_name":"renamed"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, stub.updated)

	// The value handed to the repository still carries the ciphertext, so the
	// subsequent full-row update restores it instead of writing an empty key.
	rewritten, err := stub.updated.Parameters.Value()
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rewritten.([]byte), &doc))
	require.Equal(t, "enc:v1:",
		func() string {
			key, _ := doc["api_key"].(string)
			if len(key) >= len("enc:v1:") {
				return key[:len("enc:v1:")]
			}
			return key
		}())
}
