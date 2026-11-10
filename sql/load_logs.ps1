# Copies the companion's JSON Lines logs out of its container and loads them
# into cache_insights.decisions_raw / audits_raw (and cache-ages.json into
# cache_ages_raw). Running it again REPLACES the loaded rows (TRUNCATE + COPY in
# one transaction). Needs sql\install.ps1 to have been run once.
# No new driver: it is plain `docker cp` + server-side COPY.
param(
    [string]$Companion = $(if ($env:CI_COMPANION) { $env:CI_COMPANION } else { 'hackathon-workspace-freshness-companion-1' }),
    [string]$Postgres  = $(if ($env:CI_POSTGRES)  { $env:CI_POSTGRES }  else { 'hackathon-workspace-postgres-1' }),
    [string]$DbUser = 'bifrost',
    [string]$Db     = 'bifrost'
)
$ErrorActionPreference = 'Stop'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ci-logs-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    foreach ($f in 'decisions.jsonl', 'audits.jsonl', 'cache-ages.json') {
        docker cp "${Companion}:/app/data/$f" (Join-Path $tmp $f) 2>$null | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Warning "$f not found in $Companion (skipped)"; Set-Content (Join-Path $tmp $f) '' -NoNewline }
        docker cp (Join-Path $tmp $f) "${Postgres}:/tmp/ci_$f" | Out-Null
    }
    # CSV format with control-character quote/delimiter = "one whole line into one column",
    # with no backslash or quote interpretation (JSON is full of both).
    $sql = @'
BEGIN;
TRUNCATE cache_insights.decisions_raw, cache_insights.audits_raw, cache_insights.cache_ages_raw;
COPY cache_insights.decisions_raw(doc)  FROM '/tmp/ci_decisions.jsonl' (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COPY cache_insights.audits_raw(doc)     FROM '/tmp/ci_audits.jsonl'    (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COPY cache_insights.cache_ages_raw(doc) FROM '/tmp/ci_cache-ages.json' (FORMAT csv, QUOTE E'\x01', DELIMITER E'\x02');
COMMIT;
SELECT * FROM cache_insights.log_status;
'@
    $sqlFile = Join-Path $tmp 'load.sql'
    [IO.File]::WriteAllText($sqlFile, $sql)
    docker cp $sqlFile "${Postgres}:/tmp/ci_load.sql" | Out-Null
    docker exec $Postgres psql -U $DbUser -d $Db -v ON_ERROR_STOP=1 -q -f /tmp/ci_load.sql
    if ($LASTEXITCODE -ne 0) { throw 'load failed' }
} finally {
    docker exec $Postgres sh -c 'rm -f /tmp/ci_*' 2>$null | Out-Null
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
