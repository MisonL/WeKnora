package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// preferenceOnlyUserRepo fails the test if preferences go through the
// full-row writer.
type preferenceOnlyUserRepo struct {
	interfaces.UserRepository
	user         *types.User
	fullWrites   int
	storedPrefs  types.UserPreferences
	columnWrites int
}

func (r *preferenceOnlyUserRepo) GetUserByID(context.Context, string) (*types.User, error) {
	cp := *r.user
	return &cp, nil
}

func (r *preferenceOnlyUserRepo) UpdateUser(context.Context, *types.User) error {
	r.fullWrites++
	return errors.New("full-row UpdateUser must not be used for preferences")
}

func (r *preferenceOnlyUserRepo) UpdateUserPreferencesOnly(
	_ context.Context, _ string, prefs types.UserPreferences,
) error {
	r.columnWrites++
	r.storedPrefs = prefs
	return nil
}

// Saving preferences must be a column-level UPDATE: a full-row save replays a
// stale is_system_admin/is_active snapshot and can undo a concurrent revoke.
func TestUpdateUserPreferencesWritesOnlyPreferencesColumn(t *testing.T) {
	ctx := context.Background()
	repo := &preferenceOnlyUserRepo{user: &types.User{
		ID: "u1", TenantID: 5, IsSystemAdmin: true, IsActive: true,
	}}
	svc := &userService{userRepo: repo}

	instructions := "prefer official docs"
	got, err := svc.UpdateUserPreferences(ctx, "u1", types.UserPreferences{
		BrowserSearchInstructions: &instructions,
	})
	if err != nil {
		t.Fatalf("UpdateUserPreferences: %v", err)
	}
	if repo.fullWrites != 0 {
		t.Fatalf("full-row UpdateUser calls = %d, want 0", repo.fullWrites)
	}
	if repo.columnWrites != 1 {
		t.Fatalf("column writes = %d, want 1", repo.columnWrites)
	}
	if got.BrowserSearchInstructions == nil || *got.BrowserSearchInstructions != instructions {
		t.Fatalf("merged preferences not returned: %+v", got)
	}
	if repo.storedPrefs.BrowserSearchInstructions == nil ||
		*repo.storedPrefs.BrowserSearchInstructions != instructions {
		t.Fatalf("persisted preferences = %+v", repo.storedPrefs)
	}
}
