package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
)

func crossTenantConfig(enabled bool) *config.Config {
	return &config.Config{Tenant: &config.TenantConfig{EnableCrossTenantAccess: enabled}}
}

// CanAccessAllTenants is an attribute; EnableCrossTenantAccess is the
// deployment switch that grants it meaning. With the switch closed a switch
// into a workspace without an active membership must fail.
func TestSwitchTenantSuperuserRejectedWhenCrossTenantDisabled(t *testing.T) {
	ctx := context.Background()
	repo := &switchTenantUserRepo{users: map[string]types.User{
		"alice": {ID: "alice", TenantID: 7, CanAccessAllTenants: true},
	}}
	svc := newSwitchTenantTestService(repo, &membershipLookupService{
		byTenant: map[uint64]*types.TenantMember{},
	})
	svc.config = crossTenantConfig(false)
	user, _ := repo.GetUserByID(ctx, "alice")

	_, err := svc.SwitchTenant(ctx, user, 42, "")
	if !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("switch with cross-tenant disabled must fail as membership-not-found, got %v", err)
	}
	stored, _ := repo.GetUserByID(ctx, "alice")
	if stored.Preferences.LastActiveTenantID != nil {
		t.Fatalf("rejected switch must not record a landing preference: %v",
			stored.Preferences.LastActiveTenantID)
	}
}

// The same caller succeeds once the operator enables the deployment switch.
func TestSwitchTenantSuperuserAllowedWhenCrossTenantEnabled(t *testing.T) {
	ctx := context.Background()
	repo := &switchTenantUserRepo{users: map[string]types.User{
		"alice": {ID: "alice", TenantID: 7, CanAccessAllTenants: true},
	}}
	svc := newSwitchTenantTestService(repo, &membershipLookupService{
		byTenant: map[uint64]*types.TenantMember{},
	})
	svc.config = crossTenantConfig(true)
	user, _ := repo.GetUserByID(ctx, "alice")

	if _, err := svc.SwitchTenant(ctx, user, 42, ""); err != nil {
		t.Fatalf("SwitchTenant with cross-tenant enabled: %v", err)
	}
}

// Login/refresh restoration follows the same rule: a last-active workspace
// the user is no longer a member of is not restored purely because the user
// still carries the superuser attribute, when the deployment switch is off.
func TestResolveLoginTenantIDSuperuserHonoursCrossTenantSwitch(t *testing.T) {
	ctx := context.Background()
	preferred := uint64(42)
	user := &types.User{
		ID:                  "alice",
		TenantID:            7,
		CanAccessAllTenants: true,
		Preferences:         types.UserPreferences{LastActiveTenantID: &preferred},
	}
	memberSvc := &membershipLookupService{byTenant: map[uint64]*types.TenantMember{
		7: {TenantID: 7, Status: types.TenantMemberStatusActive},
	}}

	repo := &switchTenantUserRepo{users: map[string]types.User{"alice": *user}}
	svc := newSwitchTenantTestService(repo, memberSvc)
	svc.config = crossTenantConfig(false)
	svc.tenantService = &provisioningTenantService{}

	if got := svc.resolveLoginTenantID(ctx, user); got != 7 {
		t.Fatalf("closed cross-tenant switch must fall back to home 7, got %d", got)
	}

	user = &types.User{
		ID:                  "alice",
		TenantID:            7,
		CanAccessAllTenants: true,
		Preferences:         types.UserPreferences{LastActiveTenantID: &preferred},
	}
	svc.config = crossTenantConfig(true)
	if got := svc.resolveLoginTenantID(ctx, user); got != 42 {
		t.Fatalf("open cross-tenant switch must restore workspace 42, got %d", got)
	}
}
