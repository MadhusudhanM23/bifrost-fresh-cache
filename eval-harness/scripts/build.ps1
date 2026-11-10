# Builds storebench for the Windows host (bin/storebench.exe) and for Linux
# (bin/storebench-linux, to run inside a container next to the databases), both
# from the same source state and both stamped with a fingerprint of the Bifrost
# vectorstore sources they were compiled from. The fingerprint ends up in every
# results file, so a result can always be tied to the code that produced it.
#
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1
$ErrorActionPreference = "Stop"
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
$root = Split-Path -Parent $PSScriptRoot
$vectorstore = Join-Path $root "..\bifrost\framework\vectorstore"

$parts = foreach ($file in "store.go", "pgvector.go", "qdrant.go", "redis.go", "utils.go", "errors.go") {
    $hash = (Get-FileHash -Algorithm SHA256 (Join-Path $vectorstore $file)).Hash.Substring(0, 16).ToLower()
    "$file=$hash"
}
$fingerprint = $parts -join ","
$ldflags = "-X bifrost-eval-harness/internal/envinfo.BuildFingerprint=$fingerprint"

Push-Location $root
try {
    go build -ldflags $ldflags -o bin/storebench.exe ./cmd/storebench
    if ($LASTEXITCODE -ne 0) { throw "windows build failed" }
    $env:CGO_ENABLED = "0"; $env:GOOS = "linux"; $env:GOARCH = "amd64"
    go build -ldflags $ldflags -o bin/storebench-linux ./cmd/storebench
    if ($LASTEXITCODE -ne 0) { throw "linux build failed" }
} finally {
    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
"built bin/storebench.exe and bin/storebench-linux from $fingerprint"
