package container

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Behavior: the pool is always capped. DB_MAX_OPEN_CONNS overrides the
// default; unset, zero and negative values all fall back to the default cap
// so a burst of parallel requests can never exhaust the server's
// max_connections (default cap waits instead of failing).
func TestApplyDBMaxOpenConns(t *testing.T) {
	const wantDefault = 20
	cases := []struct {
		name string
		env  string
		want int
	}{
		{name: "unset falls back to the default cap", env: "", want: wantDefault},
		{name: "positive value caps the pool", env: "40", want: 40},
		{name: "surrounding spaces are tolerated", env: " 25 ", want: 25},
		{name: "zero falls back to the default cap", env: "0", want: wantDefault},
		{name: "negative value falls back to the default cap", env: "-5", want: wantDefault},
		{name: "non-numeric value falls back to the default cap", env: "forty", want: wantDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_MAX_OPEN_CONNS", tc.env)
			// sql.Open does not dial, so no server is needed to inspect the pool.
			sqlDB, err := sql.Open("pgx", "postgres://user@127.0.0.1:1/db")
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })

			applyDBMaxOpenConns(context.Background(), sqlDB)

			if got := sqlDB.Stats().MaxOpenConnections; got != tc.want {
				t.Fatalf("MaxOpenConnections = %d, want %d", got, tc.want)
			}
		})
	}
}
