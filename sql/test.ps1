# Installs cache_insights, loads the logs, runs EVERY view, and checks the numbers
# against independent queries on the base table / the raw log files.
# Also proves the drop is clean and that the cache table is never changed.
# Usage: powershell -File sql\test.ps1      (exit code 0 = all passed)
param(
    [string]$Postgres  = $(if ($env:CI_POSTGRES)  { $env:CI_POSTGRES }  else { 'hackathon-workspace-postgres-1' }),
    [string]$Companion = $(if ($env:CI_COMPANION) { $env:CI_COMPANION } else { 'hackathon-workspace-freshness-companion-1' })
)
$ErrorActionPreference = 'Stop'
$script:fail = 0
$script:pass = 0

function Q([string]$sql) {
    $out = $sql | docker exec -i $Postgres psql -U bifrost -d bifrost -At -v ON_ERROR_STOP=1 2>&1
    if ($LASTEXITCODE -ne 0) { throw "SQL failed: $sql`n$out" }
    ($out | Out-String).Trim()
}
function Check([string]$name, $actual, $expected) {
    if ("$actual" -eq "$expected") { $script:pass++; Write-Host "PASS  $name ($actual)" }
    else { $script:fail++; Write-Host "FAIL  $name : got '$actual', expected '$expected'" -ForegroundColor Red }
}
$T = 'public."BifrostSemanticCachePlugin"'

# fingerprint of the cache table before we do anything
$fpSql = "select count(*) || ':' || coalesce(md5(string_agg(id || metadata::text, ',' order by id)), '') from $T"
$before = Q $fpSql
Write-Host "cache table before: $before"

# 1. install twice (idempotent) and load logs twice (reload replaces)
& "$PSScriptRoot\install.ps1" -Postgres $Postgres | Out-Null
& "$PSScriptRoot\install.ps1" -Postgres $Postgres | Out-Null
& "$PSScriptRoot\load_logs.ps1" -Companion $Companion -Postgres $Postgres | Out-Null
& "$PSScriptRoot\load_logs.ps1" -Companion $Companion -Postgres $Postgres | Out-Null

# 2. every view runs without error
$views = @((Q "select viewname from pg_views where schemaname='cache_insights' order by 1") -split "\r?\n" | Where-Object { $_ })
foreach ($v in $views) {
    try { $n = Q "select count(*) from cache_insights.$v"; $script:pass++; Write-Host "PASS  view $v runs ($n rows)" }
    catch { $script:fail++; Write-Host "FAIL  view $v : $_" -ForegroundColor Red }
}
Check 'at least 14 views exist' ($views.Count -ge 14) $true

# 3. numbers equal independent queries on the base table
$total = Q "select count(*) from $T"
Check 'overview.entries = COUNT(*)'            (Q 'select entries from cache_insights.overview') $total
Check 'entries view rows = COUNT(*)'           (Q 'select count(*) from cache_insights.entries') $total
Check 'direct_only = rows with no embedding'   (Q 'select direct_only_entries from cache_insights.overview') (Q "select count(*) from $T where embedding is null")
Check 'with_embedding = rows with embedding'   (Q 'select entries_with_embedding from cache_insights.overview') (Q "select count(*) from $T where embedding is not null")
Check 'expired = expires_at in the past'       (Q 'select expired_but_still_stored from cache_insights.overview') (Q "select count(*) from $T where (metadata->>'expires_at')::bigint < extract(epoch from now())")
Check 'expired + valid = total'                (Q 'select expired_but_still_stored + still_valid from cache_insights.overview') $total
Check 'sum entries_per_cache_key = COUNT(*)'   (Q 'select sum(entries) from cache_insights.entries_per_cache_key') $total
Check 'cache keys = distinct cache_key'        (Q 'select count(*) from cache_insights.entries_per_cache_key') (Q "select count(distinct metadata->>'cache_key') from $T")
Check 'sum by_model_provider = COUNT(*)'       (Q 'select sum(entries) from cache_insights.by_model_provider') $total
Check 'sum stored_size entries = COUNT(*)'     (Q 'select sum(entries) from cache_insights.stored_size') $total
Check 'table_bytes = pg_relation_size'         (Q 'select table_bytes from cache_insights.overview') (Q "select pg_relation_size('$T'::regclass)")
Check 'index_bytes = pg_indexes_size'          (Q 'select index_bytes from cache_insights.overview') (Q "select pg_indexes_size('$T'::regclass)")
Check 'oldest_entries <= 50 rows'              (Q 'select count(*) <= 50 from cache_insights.oldest_entries') 't'
Check 'oldest first'                           (Q 'select (select stored_at_estimate from cache_insights.oldest_entries limit 1) = (select min(stored_at_estimate) from cache_insights.entries)') 't'
Check 'expiring_soon rows are all not expired' (Q 'select count(*) from cache_insights.expiring_soon where minutes_left < 0') '0'
Check 'answer_chars matches length(response)'  (Q 'select sum(answer_chars) from cache_insights.entries') (Q "select sum(length(metadata->>'response')) from $T")

# 4. privacy: no view exposes a column that could hold a prompt or an answer
$bad = Q "select count(*) from information_schema.columns where table_schema='cache_insights' and table_name in (select viewname from pg_views where schemaname='cache_insights') and column_name in ('response','prompt','content','question','answer','metadata','doc','messages')"
Check 'no view has a response/prompt/content column' $bad '0'

# 5. log tables equal the files inside the companion container
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ci-test-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $tmp | Out-Null
try {
    docker cp "${Companion}:/app/data/decisions.jsonl" "$tmp\d.jsonl" | Out-Null
    docker cp "${Companion}:/app/data/audits.jsonl" "$tmp\a.jsonl" | Out-Null
    $dObjs = @(Get-Content "$tmp\d.jsonl" | Where-Object { $_.Trim() } | ForEach-Object { $_ | ConvertFrom-Json })
    $aObjs = @(Get-Content "$tmp\a.jsonl" | Where-Object { $_.Trim() } | ForEach-Object { $_ | ConvertFrom-Json })
} finally { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
Check 'decisions_raw rows = lines in decisions.jsonl' (Q 'select count(*) from cache_insights.decisions_raw') $dObjs.Count
Check 'audits_raw rows = lines in audits.jsonl'       (Q 'select count(*) from cache_insights.audits_raw') $aObjs.Count
Check 'refreshed_per_day total = refreshed in file'   (Q 'select coalesce(sum(answers_refreshed),0) from cache_insights.refreshed_per_day') (@($dObjs | Where-Object { $_.action -eq 'refreshed' }).Count)
Check 'gate_outcomes total = decisions with a gate'   (Q 'select coalesce(sum(decisions),0) from cache_insights.gate_outcomes') (@($dObjs | Where-Object { $_.gate }).Count)
$audited = @($aObjs | Where-Object { ($_.verdict -eq 'same' -or $_.verdict -eq 'different') -and $null -ne $_.similarity }).Count
Check 'wrong_match buckets cover all same/different audits' (Q 'select coalesce(sum(audits),0) from cache_insights.wrong_match_by_similarity') $audited
$wrong = @($aObjs | Where-Object { $_.verdict -eq 'different' -and $null -ne $_.similarity }).Count
Check 'wrong_match buckets wrong total'               (Q 'select coalesce(sum(wrong_matches),0) from cache_insights.wrong_match_by_similarity') $wrong
$stale = @($dObjs | Where-Object { $_.outcome -eq 'stale' -and $_.cache_id }).Count
Check 'stale_most_often total = stale decisions'      (Q 'select coalesce(sum(times_found_stale),0) from cache_insights.stale_most_often') $stale
$neverIds = Q 'select count(*) from cache_insights.entries_never_hit'
$hitIds = @(($dObjs + $aObjs) | Where-Object { $_.cache_id } | ForEach-Object { $_.cache_id } | Sort-Object -Unique)
$inList = ($hitIds | ForEach-Object { "'$_'" }) -join ','
$expectNever = Q "select count(*) from $T where id not in ($inList)"
Check 'entries_never_hit = cache rows not in any log' $neverIds $expectNever

# 6. drop is clean, reinstall works, cache table untouched
& "$PSScriptRoot\install.ps1" -Postgres $Postgres -Drop | Out-Null
Check 'schema gone after drop' (Q "select count(*) from pg_namespace where nspname='cache_insights'") '0'
& "$PSScriptRoot\install.ps1" -Postgres $Postgres | Out-Null
& "$PSScriptRoot\load_logs.ps1" -Companion $Companion -Postgres $Postgres | Out-Null
Check 'schema back after reinstall' (Q "select count(*) from pg_views where schemaname='cache_insights'") $views.Count
$after = Q $fpSql
Check 'cache table unchanged (row count + md5 of all rows)' $after $before

Write-Host "`n$($script:pass) passed, $($script:fail) failed"
if ($script:fail -gt 0) { exit 1 }
