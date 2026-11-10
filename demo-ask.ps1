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

# The receipt: one header that sums up the check. Pairs are "key=value" joined
# by "; "; strings are quoted with \" and \\ escaped, fixed words are bare.
# Parsed with [regex]::Matches so it works on Windows PowerShell 5.1.
$receipt = "$($h['X-Freshness-Receipt'])"
if ($receipt) {
    $pairs = @{}
    foreach ($m in [regex]::Matches($receipt, '(\w+)=(?:"((?:[^"\\]|\\.)*)"|([^;]*))')) {
        $value = if ($m.Groups[2].Success) { $m.Groups[2].Value -replace '\\(.)', '$1' } else { $m.Groups[3].Value.Trim() }
        $pairs[$m.Groups[1].Value] = $value
    }
    $found = if ($pairs['found']) { $pairs['found'] } else { "no value" }
    $where = if ($pairs['source']) { " at $($pairs['source'])" } else { "" }
    $ago = if ($pairs.ContainsKey('checked_ago_s')) { ", checked $($pairs['checked_ago_s'])s ago" } else { "" }
    $via = if ($pairs['provider']) { " via $($pairs['provider'])" } else { "" }
    $how = if ($pairs['evidence']) { " ($($pairs['evidence']))" } else { "" }
    Write-Host "Receipt:    $($pairs['verdict']), found $found$where$ago$via$how"
} else {
    Write-Host "Receipt:    none (no freshness check ran)"
}
$json = [Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray()) | ConvertFrom-Json
Write-Host "Answer:     $($json.choices[0].message.content)" -ForegroundColor Green
Write-Host ""
