#!/usr/bin/env bash
# Same as load_logs.ps1: copy the companion's logs out, load them into the
# staging tables (a reload REPLACES them). Needs sql/install.sh run once.
set -eu
export MSYS_NO_PATHCONV=1
CO="${CI_COMPANION:-hackathon-workspace-freshness-companion-1}"
PG="${CI_POSTGRES:-hackathon-workspace-postgres-1}"
TMP="$(mktemp -d)"
HTMP="$TMP"; command -v cygpath >/dev/null 2>&1 && HTMP="$(cygpath -m "$TMP")"   # Windows Git Bash: docker.exe needs a Windows path
trap 'docker exec "$PG" sh -c "rm -f /tmp/ci_*" >/dev/null 2>&1 || true; rm -rf "$TMP"' EXIT

for f in decisions.jsonl audits.jsonl cache-ages.json; do
  if ! docker cp "$CO:/app/data/$f" "$HTMP/$f" >/dev/null 2>&1; then
    echo "warning: $f not found in $CO (skipped)" >&2
    : > "$TMP/$f"
  fi
  docker cp "$HTMP/$f" "$PG:/tmp/ci_$f" >/dev/null
done

cat > "$TMP/load.sql" <<'SQL'
BEGIN;
TRUNCATE cache_insights.decisions_raw, cache_insights.audits_raw, cache_insights.cache_ages_raw;
COPY cache_insights.decisions_raw(doc)  FROM '/tmp/ci_decisions.jsonl' (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COPY cache_insights.audits_raw(doc)     FROM '/tmp/ci_audits.jsonl'    (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COPY cache_insights.cache_ages_raw(doc) FROM '/tmp/ci_cache-ages.json' (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COMMIT;
SELECT * FROM cache_insights.log_status;
SQL
docker cp "$HTMP/load.sql" "$PG:/tmp/ci_load.sql" >/dev/null
docker exec "$PG" psql -U bifrost -d bifrost -v ON_ERROR_STOP=1 -q -f /tmp/ci_load.sql
