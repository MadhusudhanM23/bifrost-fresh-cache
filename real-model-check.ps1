# Checks the stack against a REAL model (Grok via xAI, or Groq).
#   1. put the key in .env:  XAI_API_KEY=...  (Grok, starts "xai-")
#                         or GROQ_API_KEY=... (Groq, starts "gsk_")
#   2. docker compose -f docker-compose.yml -f docker-compose.real.yml up -d
#   3. powershell -File real-model-check.ps1            (picks a model itself)
#      powershell -File real-model-check.ps1 -Model xai/grok-3-mini
# It never prints or reads the key; it only talks to the local stack.
param(
    [string]$Model = "",
    [string]$Companion = "http://127.0.0.1:8090",
    [string]$Bifrost = "http://127.0.0.1:18090"
)
$ErrorActionPreference = "Stop"

# Models the provider reports (only listed once a working key is loaded).
$known = @()
try { $known = @((Invoke-RestMethod "$Bifrost/v1/models" -TimeoutSec 20).data.id | Where-Object { $_ -match '^(xai|groq|openrouter)/' }) } catch {}
Write-Host ("Real models Bifrost can see: {0}" -f $known.Count)
if ($Model -eq "") {
    # Small, cheap models first.
    $prefer = @("groq/openai/gpt-oss-20b", "openrouter/nvidia/nemotron-3.5-lightning:free",
                "openrouter/liquid/lfm-3-1.7b:free",
                "xai/grok-3-mini", "xai/grok-4-fast-non-reasoning", "xai/grok-4-1-fast-non-reasoning",
                "groq/llama-3.1-8b-instant", "groq/llama-3.3-70b-versatile")
    $Model = $prefer | Where-Object { $known -contains $_ } | Select-Object -First 1
    if (-not $Model -and $known.Count -gt 0) { $Model = $known[0] }
    if (-not $Model) {
        Write-Host "No real model is visible. Is the key saved in .env, and was the stack started with docker-compose.real.yml?" -ForegroundColor Red
        exit 1
    }
}
Write-Host "Using model: $Model"

$run = [int][double]::Parse((Get-Date -UFormat %s))
$key = "real-check-$run"
function Ask($text) {
    $body = @{ model = $Model; max_tokens = 800; messages = @(@{ role = "user"; content = $text }) } | ConvertTo-Json -Depth 6
    $sw = [Diagnostics.Stopwatch]::StartNew()
    try {
        $r = Invoke-WebRequest -Uri "$Companion/v1/chat/completions" -Method Post -Body $body -ContentType "application/json" -Headers @{ "x-bf-cache-key" = $key } -UseBasicParsing -TimeoutSec 90
    } catch {
        $msg = $_.ErrorDetails.Message; if (-not $msg) { $msg = $_.Exception.Message }
        Write-Host "Request failed: $msg" -ForegroundColor Red
        exit 1
    }
    $sw.Stop()
    # Windows PowerShell 5.1 decodes a reply without a charset as Latin-1; JSON is UTF-8.
    $j = [Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray()) | ConvertFrom-Json
    $hit = $r.Headers["x-bifrost-cache-hit"]; if ($hit -is [array]) { $hit = $hit[0] }
    [pscustomobject]@{ Hit = $hit; Ms = $sw.ElapsedMilliseconds; Answer = $j.choices[0].message.content }
}

$q = "In one sentence, what is a semantic cache for AI answers? ($run)"
$a = Ask $q
Write-Host ("1st ask: cache hit={0}, {1} ms" -f $a.Hit, $a.Ms)
Write-Host ("   answer: " + $a.Answer)
Start-Sleep -Seconds 2
$b = Ask $q
Write-Host ("2nd ask: cache hit={0}, {1} ms" -f $b.Hit, $b.Ms)

$ok = ($a.Answer -and $a.Hit -eq "false" -and $b.Hit -eq "true" -and $a.Answer -eq $b.Answer)
if ($ok) { Write-Host "PASS  real model answered, and the repeat came from the cache" -ForegroundColor Green }
else { Write-Host "FAIL  expected a miss then a hit with the same answer" -ForegroundColor Red; exit 1 }
