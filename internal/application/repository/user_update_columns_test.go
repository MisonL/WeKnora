package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newUserRepositoryTestDB(t *testing.T) (*gorm.DB, *userRepository) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&types.User{}))
	return db, &userRepository{db: db}
}

func seedUser(t *testing.T, db *gorm.DB, user *types.User) {
	t.Helper()
	require.NoError(t, user.Preferences.Scan([]byte(`{"last_active_tenant_id":5}`)))
	require.NoError(t, db.Create(user).Error)
}

// A preferences save must not replay stale protected columns. This is the P1
// window: the caller loaded the user, an administrator revoked system-admin in
// between, and a full-row Save would write is_system_admin=true back.
func TestUpdateUserPreferencesOnlySurvivesConcurrentRevoke(t *testing.T) {
	db, repo := newUserRepositoryTestDB(t)
	ctx := context.Background()
	user := &types.User{
		ID:            "u1",
		Username:      "alice",
		Email:         "alice@example.com",
		PasswordHash:  "old-hash",
		TenantID:      5,
		IsActive:      true,
		IsSystemAdmin: true,
	}
	seedUser(t, db, user)

	// The caller's snapshot still says "system admin, active".
	stale, err := repo.GetUserByID(ctx, "u1")
	require.NoError(t, err)
	require.True(t, stale.IsSystemAdmin)

	// Concurrent revocation.
	require.NoError(t, db.Model(&types.User{}).Where("id = ?", "u1").
		Updates(map[string]interface{}{"is_system_admin": false, "is_active": false}).Error)

	prefs := stale.Preferences
	instructions := "search help"
	prefs.BrowserSearchInstructions = &instructions
	require.NoError(t, repo.UpdateUserPreferencesOnly(ctx, "u1", prefs))

	stored, err := repo.GetUserByID(ctx, "u1")
	require.NoError(t, err)
	require.False(t, stored.IsSystemAdmin, "preference save must not resurrect system-admin")
	require.False(t, stored.IsActive, "preference save must not re-enable a disabled account")
	require.NotNil(t, stored.Preferences.BrowserSearchInstructions)
	require.Equal(t, instructions, *stored.Preferences.BrowserSearchInstructions)
}

// UpdateUser is a full-row edit of editable columns only: it must not touch
// status, admin flag, tenant, password hash or the OIDC binding.
func TestUpdateUserOmitsProtectedColumns(t *testing.T) {
	db, repo := newUserRepositoryTestDB(t)
	ctx := context.Background()
	user := &types.User{
		ID:            "u1",
		Username:      "alice",
		Email:         "alice@example.com",
		PasswordHash:  "old-hash",
		TenantID:      5,
		IsActive:      true,
		IsSystemAdmin: true,
	}
	seedUser(t, db, user)

	stale, err := repo.GetUserByID(ctx, "u1")
	require.NoError(t, err)
	stale.Avatar = "https://avatar.example.com/a.png"

	require.NoError(t, db.Model(&types.User{}).Where("id = ?", "u1").Updates(map[string]interface{}{
		"is_active":       false,
		"is_system_admin": false,
		"tenant_id":       9,
		"password_hash":   "rotated-hash",
	}).Error)

	require.NoError(t, repo.UpdateUser(ctx, stale))

	stored, err := repo.GetUserByID(ctx, "u1")
	require.NoError(t, err)
	require.Equal(t, "https://avatar.example.com/a.png", stored.Avatar, "editable profile field lands")
	require.False(t, stored.IsActive)
	require.False(t, stored.IsSystemAdmin)
	require.Equal(t, uint64(9), stored.TenantID)
	require.Equal(t, "rotated-hash", stored.PasswordHash)
}

// Column-scoped setters write exactly their column and nothing else.
func TestUserColumnSetters(t *testing.T) {
	db, repo := newUserRepositoryTestDB(t)
	ctx := context.Background()
	seedUser(t, db, &types.User{
		ID: "u1", Username: "alice", Email: "alice@example.com", PasswordHash: "hash", TenantID: 5,
	})

	require.NoError(t, repo.UpdateUserPasswordHash(ctx, "u1", "new-hash"))
	require.NoError(t, repo.UpdateUserSystemAdmin(ctx, "u1", true))
	require.NoError(t, repo.UpdateUserTenantID(ctx, "u1", 0))

	stored, err := repo.GetUserByID(ctx, "u1")
	require.NoError(t, err)
	require.Equal(t, "new-hash", stored.PasswordHash)
	require.True(t, stored.IsSystemAdmin)
	require.Zero(t, stored.TenantID)
}
