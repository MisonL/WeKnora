package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
)

// oidcBindingRepo is a tiny in-memory UserRepository for the OIDC
// account-binding tests. It implements every interface method so the
// real service graph can run without a database.
type oidcBindingRepo struct {
	users       map[string]*types.User // keyed by user id
	callsByMail int
	callsByIdn  int
}

func newOIDCBindingRepo() *oidcBindingRepo {
	return &oidcBindingRepo{users: map[string]*types.User{}}
}

func (r *oidcBindingRepo) add(u *types.User) *types.User {
	r.users[u.ID] = u
	return u
}

func (r *oidcBindingRepo) CreateUser(_ context.Context, u *types.User) error {
	r.users[u.ID] = u
	return nil
}

func (r *oidcBindingRepo) GetUserByID(_ context.Context, id string) (*types.User, error) {
	u, ok := r.users[id]
	if !ok {
		return nil, apprepo.ErrUserNotFound
	}
	return u, nil
}

func (r *oidcBindingRepo) GetUsersByIDs(context.Context, []string) (map[string]*types.User, error) {
	return nil, nil
}

func (r *oidcBindingRepo) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	r.callsByMail++
	for _, u := range r.users {
		if u.Email == email {
			return u, nil
		}
	}
	return nil, apprepo.ErrUserNotFound
}

func (r *oidcBindingRepo) GetUserByOIDCIdentity(_ context.Context, issuer, subject string) (*types.User, error) {
	r.callsByIdn++
	for _, u := range r.users {
		if u.OIDCIssuer != nil && u.OIDCSubject != nil &&
			*u.OIDCIssuer == issuer && *u.OIDCSubject == subject {
			return u, nil
		}
	}
	return nil, apprepo.ErrUserNotFound
}

func (r *oidcBindingRepo) GetUserByUsername(context.Context, string) (*types.User, error) {
	return nil, apprepo.ErrUserNotFound
}

func (r *oidcBindingRepo) GetUserByTenantID(context.Context, uint64) (*types.User, error) {
	return nil, apprepo.ErrUserNotFound
}

func (r *oidcBindingRepo) UpdateUser(_ context.Context, u *types.User) error {
	r.users[u.ID] = u
	return nil
}

func (r *oidcBindingRepo) DeleteUser(context.Context, string) error { return nil }

func (r *oidcBindingRepo) ListUsers(context.Context, int, int) ([]*types.User, error) {
	return nil, nil
}

func (r *oidcBindingRepo) ListSystemAdmins(context.Context, int, int) ([]*types.User, int64, error) {
	return nil, 0, nil
}

func (r *oidcBindingRepo) RevokeSystemAdmin(context.Context, string, string) (*types.User, error) {
	return nil, nil
}

func (r *oidcBindingRepo) SetUserActive(
	_ context.Context, userID, _ string, active bool,
) (*types.User, error) {
	u, ok := r.users[userID]
	if !ok {
		return nil, apprepo.ErrUserNotFound
	}
	u.IsActive = active
	return u, nil
}

func (r *oidcBindingRepo) SearchUsers(context.Context, string, int) ([]*types.User, error) {
	return nil, nil
}

func newOIDCBindingService(repo *oidcBindingRepo) *userService {
	return &userService{
		userRepo:  repo,
		tokenRepo: &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}},
	}
}

func oidcStrPtr(v string) *string { return &v }

func boundUser(id, email, issuer, subject string) *types.User {
	return &types.User{
		ID:           id,
		Username:     id,
		Email:        email,
		PasswordHash: "x",
		IsActive:     true,
		OIDCIssuer:   oidcStrPtr(issuer),
		OIDCSubject:  oidcStrPtr(subject),
	}
}

const testOIDCIssuer = "https://idp.example"

// A subject match logs the bound account in even when the provider now
// reports a different email. The (issuer, subject) pair is the identity;
// email must never override it.
func TestOIDCLoginMatchesBoundSubjectBeforeEmail(t *testing.T) {
	repo := newOIDCBindingRepo()
	bound := repo.add(boundUser("user-bound", "old@example.com", testOIDCIssuer, "sub-1"))
	svc := newOIDCBindingService(repo)

	resp, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:  testOIDCIssuer,
		Subject: "sub-1",
		Email:   "changed@example.com", // provider changed the address
		Claims:  map[string]interface{}{"sub": "sub-1"},
	}, types.TenantProvisioningTenantless)
	require.NoError(t, err)
	require.True(t, resp.Success)
	require.Equal(t, bound.ID, resp.User.ID)
	require.Equal(t, 0, repo.callsByMail, "a subject hit must not fall through to the email lookup")
}

// An existing local account must not be bound from an unverified email:
// whoever controls that address at the IdP could otherwise take it over.
func TestOIDCLoginRejectsUnverifiedEmailBinding(t *testing.T) {
	repo := newOIDCBindingRepo()
	existing := repo.add(&types.User{
		ID: "user-local", Username: "local", Email: "victim@example.com",
		PasswordHash: "x", IsActive: true,
	})
	svc := newOIDCBindingService(repo)

	resp, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:        testOIDCIssuer,
		Subject:       "attacker-sub",
		Email:         "victim@example.com",
		EmailVerified: false,
		Claims:        map[string]interface{}{"sub": "attacker-sub"},
	}, types.TenantProvisioningTenantless)
	require.ErrorIs(t, err, ErrOIDCEmailNotVerified)
	require.EqualError(t, err, "邮箱未验证，无法绑定已有账号")
	require.Nil(t, resp)
	require.Nil(t, existing.OIDCIssuer, "unverified login must not write a binding")
	require.Nil(t, existing.OIDCSubject)
}

// A verified email on an unbound account is bound to the presented
// identity and logged in.
func TestOIDCLoginBindsVerifiedEmailToExistingAccount(t *testing.T) {
	repo := newOIDCBindingRepo()
	existing := repo.add(&types.User{
		ID: "user-local", Username: "local", Email: "person@example.com",
		PasswordHash: "x", IsActive: true,
	})
	svc := newOIDCBindingService(repo)

	resp, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:        testOIDCIssuer,
		Subject:       "sub-verified",
		Email:         "person@example.com",
		EmailVerified: true,
		Claims:        map[string]interface{}{"sub": "sub-verified"},
	}, types.TenantProvisioningTenantless)
	require.NoError(t, err)
	require.True(t, resp.Success)
	require.Equal(t, existing.ID, resp.User.ID)
	require.NotNil(t, existing.OIDCIssuer)
	require.Equal(t, testOIDCIssuer, *existing.OIDCIssuer)
	require.NotNil(t, existing.OIDCSubject)
	require.Equal(t, "sub-verified", *existing.OIDCSubject)

	// A second login with the same pair resolves by subject, not email.
	resp2, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:        testOIDCIssuer,
		Subject:       "sub-verified",
		Email:         "person@example.com",
		EmailVerified: true,
	}, types.TenantProvisioningTenantless)
	require.NoError(t, err)
	require.Equal(t, existing.ID, resp2.User.ID)
}

// An account already bound to another subject cannot be rebound through
// the email path, even when that email is verified.
func TestOIDCLoginRejectsAlreadyBoundDifferentSubject(t *testing.T) {
	repo := newOIDCBindingRepo()
	existing := repo.add(boundUser("user-bound", "shared@example.com", testOIDCIssuer, "sub-original"))
	svc := newOIDCBindingService(repo)

	_, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:        testOIDCIssuer,
		Subject:       "sub-other",
		Email:         "shared@example.com",
		EmailVerified: true,
	}, types.TenantProvisioningTenantless)
	require.ErrorIs(t, err, ErrOIDCIdentityConflict)
	require.Equal(t, "sub-original", *existing.OIDCSubject, "existing binding must be preserved")
}

// No local account at all provisions a fresh user and binds the identity.
func TestOIDCLoginProvisionsAndBindsNewUser(t *testing.T) {
	repo := newOIDCBindingRepo()
	svc := newOIDCBindingService(repo)

	resp, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer:   testOIDCIssuer,
		Subject:  "sub-new",
		Email:    "new@example.com",
		Username: "new-person",
		Claims:   map[string]interface{}{"sub": "sub-new"},
	}, types.TenantProvisioningTenantless)
	require.NoError(t, err)
	require.True(t, resp.Success)
	require.True(t, resp.IsNewUser)
	require.NotNil(t, resp.User.OIDCIssuer)
	require.Equal(t, testOIDCIssuer, *resp.User.OIDCIssuer)
	require.NotNil(t, resp.User.OIDCSubject)
	require.Equal(t, "sub-new", *resp.User.OIDCSubject)

	stored := repo.users[resp.User.ID]
	require.NotNil(t, stored)
	require.Equal(t, "sub-new", *stored.OIDCSubject)
}

// The issuer falls back to the id_token's verified `iss` claim when the
// config does not pin issuer_url.
func TestOIDCLoginFallsBackToIssuerClaim(t *testing.T) {
	repo := newOIDCBindingRepo()
	svc := newOIDCBindingService(repo)

	resp, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Subject: "sub-iss",
		Email:   "iss@example.com",
		Claims:  map[string]interface{}{"sub": "sub-iss", "iss": testOIDCIssuer},
	}, types.TenantProvisioningTenantless)
	require.NoError(t, err)
	require.Equal(t, testOIDCIssuer, *resp.User.OIDCIssuer)
}

func TestOIDCLoginRejectsMissingSubject(t *testing.T) {
	repo := newOIDCBindingRepo()
	svc := newOIDCBindingService(repo)

	_, err := svc.loginWithOIDCUserInfo(context.Background(), &types.OIDCUserInfo{
		Issuer: testOIDCIssuer,
		Email:  "no-sub@example.com",
	}, types.TenantProvisioningTenantless)
	require.ErrorIs(t, err, ErrOIDCMissingSubject)
	require.Empty(t, repo.users)
}

func TestExtractClaimAsBool(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  bool
	}{
		{"bool true", true, true},
		{"bool false", false, false},
		{"string true", "true", true},
		{"string TRUE", "TRUE", true},
		{"string false", "false", false},
		{"string garbage", "yes please", false},
		{"number one", float64(1), true},
		{"number zero", float64(0), false},
		{"missing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]interface{}{}
			if tc.value != nil {
				claims["email_verified"] = tc.value
			}
			require.Equal(t, tc.want, extractClaimAsBool(claims, "email_verified"))
		})
	}
}
