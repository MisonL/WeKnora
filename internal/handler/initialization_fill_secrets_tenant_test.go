package handler

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

const (
	testTenantID = uint64(11)
	storedOrigin = "https://models.example.com/v1"
)

func tenantCtx(tenantID uint64) context.Context {
	return context.WithValue(context.Background(), types.TenantIDContextKey, tenantID)
}

func fillSecretsHandler(stored *types.Model) *InitializationHandler {
	return &InitializationHandler{
		modelService: &stubFillSecretsModelService{
			getModelByID: func(context.Context, string) (*types.Model, error) { return stored, nil },
		},
	}
}

// A built-in model's credential is a platform secret. Testing it against a
// caller-supplied endpoint must not ship that secret to the supplied host.
func TestFillSecretsFromStoredModel_BuiltinForeignOriginSkipsSecrets(t *testing.T) {
	stored := &types.Model{
		ID:        "m-builtin",
		TenantID:  0,
		IsBuiltin: true,
		Parameters: types.ModelParameters{
			APIKey:    "platform-key",
			AppSecret: "platform-secret",
			BaseURL:   storedOrigin,
		},
	}
	h := fillSecretsHandler(stored)
	req := &ModelTestRequest{
		ModelID: "m-builtin",
		BaseURL: "https://attacker.example.net/v1",
	}

	h.fillSecretsFromStoredModel(tenantCtx(testTenantID), req)

	require.Empty(t, req.APIKey, "built-in model key must not follow a foreign endpoint")
	require.Empty(t, req.AppSecret, "built-in app secret must not follow a foreign endpoint")
}

// Same origin (scheme + host + port) keeps the stored convenience working:
// the "Test connection" button still works for an existing built-in model.
func TestFillSecretsFromStoredModel_BuiltinSameOriginFillsSecrets(t *testing.T) {
	stored := &types.Model{
		ID:        "m-builtin",
		TenantID:  0,
		IsBuiltin: true,
		Parameters: types.ModelParameters{
			APIKey:    "platform-key",
			AppSecret: "platform-secret",
			BaseURL:   storedOrigin,
		},
	}
	h := fillSecretsHandler(stored)
	req := &ModelTestRequest{
		ModelID: "m-builtin",
		// Different path but same origin — still allowed.
		BaseURL: "https://models.example.com/other/v1",
	}

	h.fillSecretsFromStoredModel(tenantCtx(testTenantID), req)

	require.Equal(t, "platform-key", req.APIKey)
	require.Equal(t, "platform-secret", req.AppSecret)
}

// A model owned by another tenant must never lend its secret, even if a
// caller guesses the model id and keeps the stored endpoint.
func TestFillSecretsFromStoredModel_ForeignTenantModelSkipsSecrets(t *testing.T) {
	stored := &types.Model{
		ID:       "m-foreign",
		TenantID: testTenantID + 1,
		Parameters: types.ModelParameters{
			APIKey:  "other-tenant-key",
			BaseURL: storedOrigin,
		},
	}
	h := fillSecretsHandler(stored)
	req := &ModelTestRequest{ModelID: "m-foreign", BaseURL: storedOrigin}

	h.fillSecretsFromStoredModel(tenantCtx(testTenantID), req)

	require.Empty(t, req.APIKey)
}

// A tenant-owned model tested against a different origin keeps its typed
// values but receives no stored secret either.
func TestFillSecretsFromStoredModel_TenantModelForeignOriginSkipsSecrets(t *testing.T) {
	stored := &types.Model{
		ID:       "m-own",
		TenantID: testTenantID,
		Parameters: types.ModelParameters{
			APIKey:  "own-key",
			BaseURL: storedOrigin,
		},
	}
	h := fillSecretsHandler(stored)
	req := &ModelTestRequest{
		ModelID: "m-own",
		BaseURL: "http://models.example.com/v1", // scheme differs
		APIKey:  "typed-key",
	}

	h.fillSecretsFromStoredModel(tenantCtx(testTenantID), req)

	require.Equal(t, "typed-key", req.APIKey, "a value the user typed always wins")
}

// Without an authenticated tenant in context the fill must fail closed: the
// handler routes are tenant-scoped, so a missing tenant is a wiring bug, not
// an invitation to hand out stored keys.
func TestFillSecretsFromStoredModel_RequiresTenantContext(t *testing.T) {
	called := false
	h := &InitializationHandler{
		modelService: &stubFillSecretsModelService{
			getModelByID: func(context.Context, string) (*types.Model, error) {
				called = true
				return &types.Model{
					TenantID:   testTenantID,
					Parameters: types.ModelParameters{APIKey: "stored", BaseURL: storedOrigin},
				}, nil
			},
		},
	}
	req := &ModelTestRequest{ModelID: "m-own", BaseURL: storedOrigin}

	h.fillSecretsFromStoredModel(context.Background(), req)

	require.False(t, called, "must not even load a model without a tenant scope")
	require.Empty(t, req.APIKey)
}
