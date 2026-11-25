package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// Login must spend the same bcrypt work on an unknown address as on a
// known one; otherwise response latency enumerates registered emails.
// bcrypt.DefaultCost takes tens of milliseconds, so a sub-millisecond
// response means the dummy comparison was skipped.
func TestLoginUnknownEmailSpendsBcryptTime(t *testing.T) {
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)

	start := time.Now()
	resp, err := svc.Login(context.Background(), &types.LoginRequest{
		Email:    "nobody@example.com",
		Password: "Guess123!",
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.Success)
	require.Equal(t, "Invalid email or password", resp.Message)
	require.GreaterOrEqual(t, elapsed, 15*time.Millisecond,
		"unknown-user login returned without a dummy bcrypt comparison (took %s)", elapsed)
}

// The same message (and no token) is returned for a real account with a
// wrong password, so the two paths are indistinguishable by response.
func TestLoginWrongPasswordMatchesUnknownEmailShape(t *testing.T) {
	tokenRepo := &stubAuthTokenRepo{tokens: map[string]*types.AuthToken{}}
	svc := newAuthTestUserService(tokenRepo)
	svc.userRepo.(*stubUserRepoForAuth).users["user-1"] = persistedTestUser(
		t, 1, "known@example.com", "Correct9!")

	unknown, err := svc.Login(context.Background(), &types.LoginRequest{
		Email: "nobody@example.com", Password: "Wrong123!",
	})
	require.NoError(t, err)
	wrong, err := svc.Login(context.Background(), &types.LoginRequest{
		Email: "known@example.com", Password: "Wrong123!",
	})
	require.NoError(t, err)

	require.Equal(t, unknown.Success, wrong.Success)
	require.Equal(t, unknown.Message, wrong.Message)
	require.Empty(t, unknown.Token)
	require.Empty(t, wrong.Token)
}
