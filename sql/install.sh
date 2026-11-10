#!/usr/bin/env bash
# Installs (or re-installs) the cache_insights schema into the demo database.
# Usage: sh sql/install.sh          (add "drop" to remove it again)
set -eu
export MSYS_NO_PATHCONV=1   # Git Bash on Windows: do not rewrite /tmp paths
PG="${CI_POSTGRES:-hackathon-workspace-postgres-1}"
DIR="$(cd "$(dirname "$0")" && pwd)"
# Windows Git Bash: docker.exe needs a Windows-style path
command -v cygpath >/dev/null 2>&1 && DIR="$(cygpath -m "$DIR")"
FILE=cache_insights.sql
[ "${1:-}" = "drop" ] && FILE=drop.sql
docker cp "$DIR/$FILE" "$PG:/tmp/$FILE" >/dev/null
docker exec "$PG" psql -U bifrost -d bifrost -v ON_ERROR_STOP=1 -q -f "/tmp/$FILE"
docker exec "$PG" rm -f "/tmp/$FILE"
[ "$FILE" = drop.sql ] && echo "cache_insights removed." || echo "cache_insights installed."
