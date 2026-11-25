package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/golang-jwt/jwt/v5"
)

// Access and refresh tokens used to be byte-identical when minted in the same
// second (iat/exp are second-granular), which collides in the token table and
// in any replay-window reasoning. A per-token jti keeps same-second mints
// distinct.
func TestGenerateTokensSameSecondAreUnique(t *testing.T) {
	tokenRepo := &stubAuthTokenRepo{}
	svc := &userService{
		userRepo: &stubUserRepoForAuth{users: map[string]*types.User{
			"user-1": {ID: "user-1", TenantID: 1, IsActive: true},
		}},
		tokenRepo: tokenRepo,
	}
	user := &types.User{ID: "user-1", Email: "alice@example.com", TenantID: 1}

	access1, refresh1, err := svc.generateTokensForTenant(context.Background(), user, 1)
	if err != nil {
		t.Fatalf("first token pair: %v", err)
	}
	access2, refresh2, err := svc.generateTokensForTenant(context.Background(), user, 1)
	if err != nil {
		t.Fatalf("second token pair: %v", err)
	}

	if access1 == access2 {
		t.Fatal("access tokens minted in the same second must differ")
	}
	if refresh1 == refresh2 {
		t.Fatal("refresh tokens minted in the same second must differ")
	}

	for name, raw := range map[string]string{"access": access1, "refresh": refresh1} {
		parsed, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
		if err != nil {
			t.Fatalf("parse %s token: %v", name, err)
		}
		claims, ok := parsed.Claims.(jwt.MapClaims)
		if !ok {
			t.Fatalf("unexpected claims type for %s token", name)
		}
		jti, _ := claims["jti"].(string)
		if len(jti) != 32 {
			t.Fatalf("%s token jti = %q, want 32 hex chars from 16 random bytes", name, jti)
		}
	}
}
