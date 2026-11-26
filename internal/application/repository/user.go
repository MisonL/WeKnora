package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrCannotDisableSelf prevents administrators from disabling their own account.
	ErrCannotDisableSelf = errors.New("cannot disable your own account")
	// ErrCannotDisableLastAdmin preserves at least one active system administrator.
	ErrCannotDisableLastAdmin = errors.New("cannot disable the last system administrator")
	// ErrUserNotFound indicates that the requested user does not exist.
	ErrUserNotFound = errors.New("user not found")
	// ErrUserAlreadyExists indicates a duplicate user account.
	ErrUserAlreadyExists = errors.New("user already exists")
	// ErrTokenNotFound indicates that the requested token does not exist.
	ErrTokenNotFound = errors.New("token not found")
	// ErrCannotRevokeSelf prevents administrators from revoking their own role.
	ErrCannotRevokeSelf = errors.New("cannot revoke your own system admin privileges")
	// ErrLastSystemAdmin prevents revoking the last active system administrator's role.
	ErrLastSystemAdmin = errors.New("cannot revoke the last remaining system administrator")
	// ErrUserNotSystemAdmin indicates that the user has no system-admin role to revoke.
	ErrUserNotSystemAdmin = errors.New("user is not a system administrator")
)

// userRepository implements user repository interface
type userRepository struct {
	db *gorm.DB
}

// NewUserRepository creates a new user repository
func NewUserRepository(db *gorm.DB) interfaces.UserRepository {
	return &userRepository{db: db}
}

// CreateUser creates a user
func (r *userRepository) CreateUser(ctx context.Context, user *types.User) error {
	// users.tenant_id is nullable in both PostgreSQL and SQLite. GORM would
	// otherwise serialise the uint64 zero value as 0, which violates the
	// PostgreSQL FK and loses the distinction between "not provisioned yet"
	// and a real tenant. Omitting the column stores SQL NULL; reads hydrate it
	// back as zero, the domain sentinel used by tenantless auth flows.
	if user != nil && user.TenantID == 0 {
		return r.db.WithContext(ctx).Omit("tenant_id").Create(user).Error
	}
	return r.db.WithContext(ctx).Create(user).Error
}

// GetUserByID gets a user by ID
func (r *userRepository) GetUserByID(ctx context.Context, id string) (*types.User, error) {
	var user types.User
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

// GetUsersByIDs batch-fetches users by id with a single SELECT … WHERE id IN (…)
// and projects the result into a map keyed by user id. Returns an empty
// map for an empty input slice. Missing ids are silently absent from
// the result (consistent with the interface contract used by tenant
// member hydration).
func (r *userRepository) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*types.User, error) {
	out := make(map[string]*types.User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var users []*types.User
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// GetUserByEmail gets a user by email
func (r *userRepository) GetUserByEmail(ctx context.Context, email string) (*types.User, error) {
	var user types.User
	if err := r.db.WithContext(ctx).Where("email = ?", email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

// GetUserByOIDCIdentity finds the account bound to (issuer, subject).
// Empty inputs never match: a local account stores NULL/empty in both
// columns and must not be claimed by an OIDC provider that omitted one
// of the two claims.
func (r *userRepository) GetUserByOIDCIdentity(ctx context.Context, issuer, subject string) (*types.User, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return nil, ErrUserNotFound
	}
	var user types.User
	err := r.db.WithContext(ctx).
		Where("oidc_issuer = ? AND oidc_subject = ?", issuer, subject).
		First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

// GetUserByUsername gets a user by username
func (r *userRepository) GetUserByUsername(ctx context.Context, username string) (*types.User, error) {
	var user types.User
	if err := r.db.WithContext(ctx).Where("username = ?", username).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

// GetUserByTenantID gets the first user (owner) of a tenant
func (r *userRepository) GetUserByTenantID(ctx context.Context, tenantID uint64) (*types.User, error) {
	var user types.User
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at ASC").First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

// protectedUserColumns are never written by a full-row UpdateUser. They are
// authorization/identity state that only dedicated, guarded operations may
// change: status via SetUserActive, the platform-admin flag via
// UpdateUserSystemAdmin/RevokeSystemAdmin, tenant membership via
// UpdateUserTenantID, credentials via UpdateUserPasswordHash, and OIDC
// binding via UpdateUserOIDCIdentity.
//
// Omitting them is what keeps a read-modify-write from resurrecting a value a
// concurrent administrator just revoked — the exact P1 where saving UI
// preferences rewrote is_system_admin=true from a stale in-memory user.
var protectedUserColumns = []string{
	"is_active",
	"is_system_admin",
	"tenant_id",
	"password_hash",
	"oidc_issuer",
	"oidc_subject",
	"created_at",
}

// UpdateUser updates the editable profile/preferences fields of user data.
// Protected columns (see protectedUserColumns) are never touched; use the
// dedicated setters for them.
func (r *userRepository) UpdateUser(ctx context.Context, user *types.User) error {
	return r.db.WithContext(ctx).Omit(protectedUserColumns...).Save(user).Error
}

// UpdateUserPreferencesOnly writes the preferences column alone, so an
// unrelated concurrent change to account status, admin flag, tenant or
// password survives the write.
func (r *userRepository) UpdateUserPreferencesOnly(
	ctx context.Context, userID string, prefs types.UserPreferences,
) error {
	return r.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"preferences": prefs,
			"updated_at":  time.Now(),
		}).Error
}

// UpdateUserTenantID writes only users.tenant_id. tenantID 0 stores NULL so a
// tenantless account never violates the tenant foreign key.
func (r *userRepository) UpdateUserTenantID(ctx context.Context, userID string, tenantID uint64) error {
	var value interface{}
	if tenantID == 0 {
		value = nil
	} else {
		value = tenantID
	}
	return r.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"tenant_id":  value,
			"updated_at": time.Now(),
		}).Error
}

// UpdateUserPasswordHash writes only password_hash.
func (r *userRepository) UpdateUserPasswordHash(
	ctx context.Context, userID, passwordHash string,
) error {
	return r.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"password_hash": passwordHash,
			"updated_at":    time.Now(),
		}).Error
}

// UpdateUserSystemAdmin writes only is_system_admin. The last-admin and
// self-revoke guards live in RevokeSystemAdmin; this setter is the promotion
// counterpart used by the SystemAdmin-gated handler.
func (r *userRepository) UpdateUserSystemAdmin(
	ctx context.Context, userID string, isSystemAdmin bool,
) error {
	return r.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"is_system_admin": isSystemAdmin,
			"updated_at":      time.Now(),
		}).Error
}

// UpdateUserOIDCIdentity writes only the OIDC binding columns, and only for a
// row that is still unbound or already carries this exact identity: two
// requests that both saw an unbound snapshot must not take turns overwriting
// each other's binding. The boolean reports whether this call won that
// transition.
func (r *userRepository) UpdateUserOIDCIdentity(
	ctx context.Context, userID, issuer, subject string,
) (bool, error) {
	res := r.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ?", userID).
		Where(`(oidc_issuer IS NULL AND oidc_subject IS NULL) OR (oidc_issuer = ? AND oidc_subject = ?)`,
			issuer, subject).
		Updates(map[string]interface{}{
			"oidc_issuer":  issuer,
			"oidc_subject": subject,
			"updated_at":   time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeleteUser deletes a user
func (r *userRepository) DeleteUser(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&types.User{}).Error
}

// DeleteUserRetiringSessions locks the web user, retires every session visible
// under that lock, and soft-deletes the user in the same transaction. A user
// row that is already soft-deleted is still held for the re-read; its sessions
// are retired and the call returns nil.
func (r *userRepository) DeleteUserRetiringSessions(ctx context.Context, id string, at time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := lockLiveWebUser(tx, id)
		if err != nil && !errors.Is(err, errWebUserDeleted) {
			return err
		}
		ids, err := ownerSessionIDs(tx, id)
		if err != nil {
			return err
		}
		if _, err := RetireSessionsTx(tx, ids, at); err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&types.User{}).Error
	})
}

// ListUsers lists users with pagination
func (r *userRepository) ListUsers(ctx context.Context, offset, limit int) ([]*types.User, error) {
	var users []*types.User
	query := r.db.WithContext(ctx).Order("created_at DESC")

	if limit > 0 {
		query = query.Limit(limit)
	}

	if offset > 0 {
		query = query.Offset(offset)
	}

	if err := query.Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// ListSystemAdmins lists users where is_system_admin = true.
//
// Walks idx_users_is_system_admin (created in migration 000052), so the
// query stays cheap even on a large users table — only the small subset
// of system admins is scanned. Returns total count alongside the page so
// the management UI can render pagination without a second roundtrip.
//
// Ordered by created_at DESC for stable, newest-first listing; ties are
// further broken by id to keep paging deterministic across boundaries.
// limit <= 0 means "no limit" (matches ListUsers semantics); callers in
// production pass a sane page size.
func (r *userRepository) ListSystemAdmins(ctx context.Context, offset, limit int) ([]*types.User, int64, error) {
	var users []*types.User
	var total int64

	base := r.db.WithContext(ctx).Model(&types.User{}).Where("is_system_admin = ?", true)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	query := base.Order("created_at DESC, id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}
	if err := query.Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// lockSystemAdmins uses the same lock order for status changes and role
// revocation: all admin rows by ID, then the target user. SQLite has no row
// locks, so acquire its writer lock before reading the administrator set.
func lockSystemAdmins(tx *gorm.DB) ([]types.User, error) {
	if tx.Name() == "sqlite" {
		if err := tx.Model(&types.User{}).Where("is_system_admin = ?", true).
			UpdateColumn("is_system_admin", true).Error; err != nil {
			return nil, err
		}
	}
	var admins []types.User
	err := lockUserRows(tx).Where("is_system_admin = ?", true).Order("id ASC").Find(&admins).Error
	return admins, err
}

func lockUserRows(tx *gorm.DB) *gorm.DB {
	switch tx.Name() {
	case "postgres", "mysql":
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	default:
		return tx
	}
}

func activeSystemAdminCount(admins []types.User) int {
	n := 0
	for _, admin := range admins {
		if admin.IsActive {
			n++
		}
	}
	return n
}

// SetUserActive persists status and session revocation in one transaction.
// Locking the admin set also serializes this operation with role revocation.
func (r *userRepository) SetUserActive(ctx context.Context, userID, actorID string, active bool) (*types.User, error) {
	var user types.User
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		admins, err := lockSystemAdmins(tx)
		if err != nil {
			return err
		}
		if err := lockUserRows(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		if user.IsActive == active {
			return nil
		}
		if !active {
			if userID == actorID {
				return ErrCannotDisableSelf
			}
			if user.IsSystemAdmin && activeSystemAdminCount(admins) <= 1 {
				return ErrCannotDisableLastAdmin
			}
		}
		user.IsActive = active
		user.UpdatedAt = time.Now()
		if err := tx.Model(&user).Updates(map[string]any{
			"is_active": active, "updated_at": user.UpdatedAt,
		}).Error; err != nil {
			return err
		}
		if !active {
			return tx.Model(&types.AuthToken{}).Where("user_id = ?", userID).
				Update("is_revoked", true).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// RevokeSystemAdmin revokes system-admin privileges inside a transaction.
// It locks the current admin rows before counting so concurrent revokes
// cannot both observe "two admins" and leave the platform with zero.
//
// Return contract:
//   - (user, nil): revoke actually happened; user.IsSystemAdmin == false
//   - (user, ErrUserNotSystemAdmin): target was already not an admin;
//     no row was written. Caller should treat as idempotent success but
//     MUST distinguish it from a real revoke for audit purposes — the
//     surfaced `user` is the unchanged DB row.
//   - (nil, ErrCannotRevokeSelf | ErrLastSystemAdmin | ErrUserNotFound | …):
//     hard rejection; no row written.
func (r *userRepository) RevokeSystemAdmin(ctx context.Context, userID, actorID string) (*types.User, error) {
	if userID == actorID {
		return nil, ErrCannotRevokeSelf
	}

	var revoked *types.User
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		admins, err := lockSystemAdmins(tx)
		if err != nil {
			return err
		}
		var user types.User
		if err := lockUserRows(tx).
			Where("id = ?", userID).
			First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		if !user.IsSystemAdmin {
			revoked = &user
			return ErrUserNotSystemAdmin
		}

		if len(admins) <= 1 || (user.IsActive && activeSystemAdminCount(admins) <= 1) {
			return ErrLastSystemAdmin
		}

		user.IsSystemAdmin = false
		if err := tx.Model(&user).Update("is_system_admin", false).Error; err != nil {
			return err
		}
		revoked = &user
		return nil
	})
	// Propagate ErrUserNotSystemAdmin up to the handler alongside the
	// (unchanged) user row. The handler treats it as idempotent success
	// but emits an audit row with changed=false so a probing pattern
	// ("revoke every random user id we know") still leaves a trail.
	if errors.Is(err, ErrUserNotSystemAdmin) {
		return revoked, err
	}
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// SearchUsers searches users by username or email
func (r *userRepository) SearchUsers(ctx context.Context, query string, limit int) ([]*types.User, error) {
	var users []*types.User
	// Dialect-aware bits so the same query works on Postgres and SQLite (Lite
	// build), which has no ILIKE. The keyword is escaped either way: % and _
	// are LIKE wildcards, so an unescaped search for "a_b" also matches "axb"
	// and "%" returns every user.
	isPostgres := r.db.Name() == "postgres"
	userLike := "LOWER(username) LIKE LOWER(?) ESCAPE ?"
	mailLike := "LOWER(email) LIKE LOWER(?) ESCAPE ?"
	if isPostgres {
		userLike = "username ILIKE ? ESCAPE ?"
		mailLike = "email ILIKE ? ESCAPE ?"
	}
	escaped := escapeLikeKeyword(query)
	searchPattern := "%" + escaped + "%"

	dbQuery := r.db.WithContext(ctx).
		Where(userLike+" OR "+mailLike, searchPattern, likeEscapeChar, searchPattern, likeEscapeChar).
		Where("is_active = ?", true).
		Order("username ASC")

	if limit > 0 {
		dbQuery = dbQuery.Limit(limit)
	} else {
		dbQuery = dbQuery.Limit(20) // default limit
	}

	if err := dbQuery.Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// authTokenRepository implements auth token repository interface
type authTokenRepository struct {
	db *gorm.DB
}

// NewAuthTokenRepository creates a new auth token repository
func NewAuthTokenRepository(db *gorm.DB) interfaces.AuthTokenRepository {
	return &authTokenRepository{db: db}
}

// CreateToken creates an auth token
func (r *authTokenRepository) CreateToken(ctx context.Context, token *types.AuthToken) error {
	return r.db.WithContext(ctx).Create(token).Error
}

// GetTokenByValue gets a token by its value
func (r *authTokenRepository) GetTokenByValue(ctx context.Context, tokenValue string) (*types.AuthToken, error) {
	var token types.AuthToken
	if err := r.db.WithContext(ctx).Where("token = ?", tokenValue).First(&token).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}
	return &token, nil
}

// GetTokenByID gets a token by its primary key
func (r *authTokenRepository) GetTokenByID(ctx context.Context, id string) (*types.AuthToken, error) {
	var token types.AuthToken
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&token).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}
	return &token, nil
}

// GetTokensByUserID gets all tokens for a user
func (r *authTokenRepository) GetTokensByUserID(ctx context.Context, userID string) ([]*types.AuthToken, error) {
	var tokens []*types.AuthToken
	if err := r.db.WithContext(ctx).Where("user_id = ?", userID).Find(&tokens).Error; err != nil {
		return nil, err
	}
	return tokens, nil
}

// UpdateToken updates a token
func (r *authTokenRepository) UpdateToken(ctx context.Context, token *types.AuthToken) error {
	return r.db.WithContext(ctx).Save(token).Error
}

// DeleteToken deletes a token
func (r *authTokenRepository) DeleteToken(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&types.AuthToken{}).Error
}

// DeleteExpiredTokens deletes all expired tokens
func (r *authTokenRepository) DeleteExpiredTokens(ctx context.Context) error {
	return r.db.WithContext(ctx).Where("expires_at < NOW()").Delete(&types.AuthToken{}).Error
}

// RevokeTokenIfActive flips is_revoked only when the row is currently
// active. The conditional UPDATE is the atomic compare-and-swap behind
// refresh-token rotation: of two concurrent refreshes holding the same
// token, exactly one observes RowsAffected == 1 and proceeds; the other
// observes 0 and is treated as token reuse.
func (r *authTokenRepository) RevokeTokenIfActive(ctx context.Context, tokenValue string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&types.AuthToken{}).
		Where("token = ? AND is_revoked = ?", tokenValue, false).
		UpdateColumn("is_revoked", true)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// RevokeTokensByUserID revokes all tokens for a user
func (r *authTokenRepository) RevokeTokensByUserID(ctx context.Context, userID string) error {
	return r.db.WithContext(ctx).Model(&types.AuthToken{}).Where("user_id = ?", userID).Update("is_revoked", true).Error
}
