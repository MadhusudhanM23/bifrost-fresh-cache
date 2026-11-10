# Asks the demo price question through the freshness companion and prints what
# matters: was it a cache hit, what did the freshness check decide, and the answer.
# Usage:  powershell -File demo-ask.ps1            (same cache key every time: "video-demo")
#         powershell -File demo-ask.ps1 -Key fresh1 (a new key = a brand-new conversation)
param([string]$Key = "video-demo", [string]$Companion = "http://127.0.0.1:8090")
$body = Get-Content "$PSScriptRoot\docker\request-49.json" -Raw
$r = Invoke-WebRequest "$Companion/v1/chat/completions" -Method Post -ContentType "application/json" `
        -Headers @{ "x-bf-cache-key" = $Key } -Body $body -UseBasicParsing
$h = $r.Headers
$hit = if ("$($h['X-Bifrost-Cache-Hit'])" -eq "true") { "HIT (answer came from the cache)" } else { "MISS (the model was asked)" }
Write-Host ""
Write-Host "Cache:      $hit"
Write-Host "Freshness:  status=$($h['X-Freshness-Status'])  action=$($h['X-Freshness-Action'])  via=$($h['X-Freshness-Provider'])"
if ($h['X-Freshness-Source']) { Write-Host "Checked:    $($h['X-Freshness-Source'])" }
$json = [Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray()) | ConvertFrom-Json
Write-Host "Answer:     $($json.choices[0].message.content)" -ForegroundColor Green
Write-Host ""
