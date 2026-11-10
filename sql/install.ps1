# Installs (or re-installs) the cache_insights schema into the demo database.
# Usage:  powershell -File sql\install.ps1            (add -Drop to remove it again)
param(
    [string]$Postgres = $(if ($env:CI_POSTGRES) { $env:CI_POSTGRES } else { 'hackathon-workspace-postgres-1' }),
    [string]$DbUser   = 'bifrost',
    [string]$Db       = 'bifrost',
    [switch]$Drop
)
$ErrorActionPreference = 'Stop'
$file = if ($Drop) { 'drop.sql' } else { 'cache_insights.sql' }
$src = Join-Path $PSScriptRoot $file
docker cp $src "${Postgres}:/tmp/$file" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "docker cp failed (is container $Postgres running?)" }
docker exec $Postgres psql -U $DbUser -d $Db -v ON_ERROR_STOP=1 -q -f "/tmp/$file"
if ($LASTEXITCODE -ne 0) { throw "psql failed" }
docker exec $Postgres rm -f "/tmp/$file" | Out-Null
if ($Drop) { Write-Host 'cache_insights removed.' } else { Write-Host 'cache_insights installed.' }
