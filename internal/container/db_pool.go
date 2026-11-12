package container

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
)

// defaultDBMaxOpenConns is the pool cap applied when DB_MAX_OPEN_CONNS is
// unset or invalid. database/sql's unlimited default lets a burst of parallel
// requests open one connection each until Postgres starts refusing them past
// max_connections ("sorry, too many clients already"); a capped pool makes
// excess requests wait for a free connection instead. 20 is ample for the
// request path (GORM serializes through the pool) and leaves headroom under
// typical Postgres max_connections.
const defaultDBMaxOpenConns = 20

// applyDBMaxOpenConns caps the main database pool at DB_MAX_OPEN_CONNS,
// falling back to defaultDBMaxOpenConns when the variable is unset, zero,
// negative or non-numeric. A cap of 0 (unlimited) is intentionally no longer
// reachable via the environment: operators who need a different ceiling set a
// positive value instead.
func applyDBMaxOpenConns(ctx context.Context, sqlDB *sql.DB) {
	n := defaultDBMaxOpenConns
	if raw := strings.TrimSpace(os.Getenv("DB_MAX_OPEN_CONNS")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			logger.Warnf(ctx, "Ignoring invalid DB_MAX_OPEN_CONNS=%q: want a non-negative integer; using default cap %d", raw, defaultDBMaxOpenConns)
		} else if v > 0 {
			n = v
		} else {
			logger.Infof(ctx, "DB_MAX_OPEN_CONNS=0 no longer means unlimited; using default cap %d", defaultDBMaxOpenConns)
		}
	} else {
		logger.Infof(ctx, "DB_MAX_OPEN_CONNS unset; using default cap %d open connections", defaultDBMaxOpenConns)
	}
	sqlDB.SetMaxOpenConns(n)
	logger.Infof(ctx, "Database connection pool capped at %d open connections", n)
}
