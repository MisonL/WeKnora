#!/usr/bin/env bash
# Production wrapper for the QDAMA Nora stack.
#
# Every service in docker-compose.yml that this deployment customises (app and
# frontend) lives in docker-compose.app-oss.yml, which Compose does not load
# automatically. Running a bare `docker compose up -d` would silently recreate
# those containers from the official images and drop the STS / FAQ fixes.
#
# Install as /opt/nora/bin/nora-up.sh and always start or update the stack
# through it:
#   bin/nora-up.sh up -d
#   bin/nora-up.sh up -d --no-deps --force-recreate app frontend
#   bin/nora-up.sh config
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

files=(
  docker-compose.yml
  docker-compose.override.yml
  docker-compose.app-oss.yml
)
for f in "${files[@]}"; do
  if [[ ! -f "$f" ]]; then
    echo "nora-up: required compose file is missing: $ROOT/$f" >&2
    exit 1
  fi
done

args=(-p nora)
for f in "${files[@]}"; do
  args+=(-f "$f")
done

exec docker compose "${args[@]}" "$@"
