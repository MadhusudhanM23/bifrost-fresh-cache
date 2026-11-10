# Convenience wrapper for the Go e2e runner in this directory.
# Builds the two binaries if they are missing (same commands as README.md), then runs
# `go run .` with any extra arguments, e.g.  .\run-e2e.ps1 -extras=false -authprobe=false
# The runner itself writes e2e-run.log and logs\*.log.
$ErrorActionPreference = "Stop"
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
$e2e = $PSScriptRoot
$ws = Split-Path $e2e -Parent

if (-not (Test-Path "$e2e\bin\mock-openai.exe")) {
    Write-Host "building mock-openai ..."
    Push-Location "$ws\tools\mock-openai"
    $env:CGO_ENABLED = "0"
    go build -o "$e2e\bin\mock-openai.exe" .
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "mock build failed" }
    Pop-Location
}

if (-not (Test-Path "$e2e\bin\bifrost-http.exe")) {
    Write-Host "building bifrost-http from the fork (takes a few minutes the first time) ..."
    $ui = "$ws\bifrost\transports\bifrost-http\ui"
    if (-not (Test-Path "$ui\index.html")) {
        # gitignored (/transports/bifrost-http/ui/), needed only by //go:embed all:ui
        New-Item -ItemType Directory -Force $ui | Out-Null
        '<!doctype html><html><head><meta charset="utf-8"><title>Bifrost (placeholder UI)</title></head><body><p>Placeholder UI for a local e2e build.</p></body></html>' |
            Out-File -Encoding utf8 "$ui\index.html"
    }
    Push-Location "$ws\bifrost\transports\bifrost-http"
    $env:GOWORK = "$e2e\build\go.work"
    $env:CGO_ENABLED = "0"
    go build -ldflags="-X main.Version=v0.0.0-e2e-pgvector" -o "$e2e\bin\bifrost-http.exe" .
    $code = $LASTEXITCODE
    Remove-Item Env:\GOWORK
    Pop-Location
    if ($code -ne 0) { throw "gateway build failed" }
}

Push-Location $e2e
try {
    go run . @args
    $code = $LASTEXITCODE
} finally {
    Pop-Location
}
exit $code
