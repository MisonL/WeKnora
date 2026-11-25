package service

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func activeRefreshFixture(t *testing.T) (*userService, *stubAuthTokenRepo, *types.User, string) {
	t.Helper()
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)
	user := &types.User{ID: "user-1", TenantID: 1, IsActive: true}
	svc.userRepo.(*stubUserRepoForAuth).users["user-1"] = user

	refresh := signTestJWT(jwt.MapClaims{
		"user_id": "user-1",
		"type":    "refresh",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	tokenRepo.tokens[refresh] = &types.AuthToken{
		ID:        "tok-old",
		UserID:    "user-1",
		Token:     refresh,
		TokenType: "refresh_token",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	return svc, tokenRepo, user, refresh
}

// A disabled account must not roll a refresh token forward even if the
// token row itself was never revoked (e.g. the revoke write failed).
func TestRefreshTokenRejectsDisabledAccount(t *testing.T) {
	ctx := context.Background()
	svc, tokenRepo, user, refresh := activeRefreshFixture(t)
	user.IsActive = false

	_, _, err := svc.RefreshToken(ctx, refresh)
	require.Error(t, err)
	require.Contains(t, err.Error(), "account is disabled")
	require.False(t, tokenRepo.tokens[refresh].IsRevoked,
		"the token must not be consumed by a rejected refresh")
}

// The happy path rotates: the presented token is atomically revoked and a
// fresh pair is persisted before it is returned.
func TestRefreshTokenRotatesOldTokenAtomically(t *testing.T) {
	ctx := context.Background()
	svc, tokenRepo, _, refresh := activeRefreshFixture(t)

	access, newRefresh, err := svc.RefreshToken(ctx, refresh)
	require.NoError(t, err)
	require.NotEmpty(t, access)
	require.NotEmpty(t, newRefresh)
	require.NotEqual(t, refresh, newRefresh)
	require.True(t, tokenRepo.tokens[refresh].IsRevoked,
		"the old refresh token must be revoked")
	require.Contains(t, tokenRepo.tokens, access)
	require.Contains(t, tokenRepo.tokens, newRefresh)
}

// Two refreshes racing on the same token: the loser observes that the
// conditional revoke matched nothing. That is replay evidence, so every
// session of the user is revoked and the refresh is refused.
func TestRefreshTokenReuseRevokesAllSessions(t *testing.T) {
	ctx := context.Background()
	svc, tokenRepo, _, refresh := activeRefreshFixture(t)
	tokenRepo.revokeIfActiveFails = true

	_, _, err := svc.RefreshToken(ctx, refresh)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reuse")
	require.Equal(t, []string{"user-1"}, tokenRepo.revokedUserIDs)
}

func TestValidateTokenRejectsMissingExpiration(t *testing.T) {
	ctx := context.Background()
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)

	noExp := signTestJWT(jwt.MapClaims{"user_id": "user-1", "type": "access"})
	tokenRepo.tokens[noExp] = &types.AuthToken{UserID: "user-1", Token: noExp, TokenType: "access_token"}

	_, _, err := svc.ValidateToken(ctx, noExp)
	require.Error(t, err)
	require.Equal(t, "invalid token", err.Error())
}

func TestValidateTokenRejectsNonHS256(t *testing.T) {
	ctx := context.Background()
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)

	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"user_id": "user-1",
		"type":    "access",
		"exp":     time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(getJwtSecret()))
	require.NoError(t, err)
	tokenRepo.tokens[hs512] = &types.AuthToken{UserID: "user-1", Token: hs512, TokenType: "access_token"}

	_, _, err = svc.ValidateToken(ctx, hs512)
	require.Error(t, err)
	require.Equal(t, "invalid token", err.Error())
}

func TestRefreshTokenRejectsMissingExpiration(t *testing.T) {
	ctx := context.Background()
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)

	noExp := signTestJWT(jwt.MapClaims{"user_id": "user-1", "type": "refresh"})
	tokenRepo.tokens[noExp] = &types.AuthToken{UserID: "user-1", Token: noExp, TokenType: "refresh_token"}

	_, _, err := svc.RefreshToken(ctx, noExp)
	require.Error(t, err)
	require.Equal(t, "invalid refresh token", err.Error())
}

// Logout tolerates an expired token (so users can end a stale session),
// but a token with no exp at all is malformed and must be rejected.
func TestLogoutRejectsMissingExpiration(t *testing.T) {
	ctx := context.Background()
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)

	noExp := signTestJWT(jwt.MapClaims{"user_id": "user-1", "type": "access"})
	require.Error(t, svc.Logout(ctx, noExp))
	require.Empty(t, tokenRepo.revokedUserIDs)

	expired := signTestJWT(jwt.MapClaims{
		"user_id": "user-1", "type": "access", "exp": time.Now().Add(-time.Hour).Unix(),
	})
	require.NoError(t, svc.Logout(ctx, expired))
}

func TestParseSandboxTerminalTicketRejectsMissingExpiration(t *testing.T) {
	ticket := signTestJWT(jwt.MapClaims{
		"user_id": "user-1", "tenant_id": 1, "session_id": "s", "token_id": "t",
		"type": sandboxTerminalTicketType,
	})
	_, err := ParseSandboxTerminalTicket(ticket)
	require.Error(t, err)
}
