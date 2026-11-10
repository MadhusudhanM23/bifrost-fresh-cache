# One-command check that the running Docker stack works end to end.
#   docker compose up --build -d      (wait until healthy)
#   powershell -File smoke.ps1
# It sends real requests through the companion on :8090 and checks the answers
# and headers. Everything uses unique cache keys and changes nothing on disk.
# It uses the MOCK model, so it proves the mechanics, not real-model quality.
param([string]$Companion = "http://127.0.0.1:8090", [string]$Bifrost = "http://127.0.0.1:18090")
$ErrorActionPreference = "Stop"
$script:pass = 0; $script:fail = 0
function Check($name, $ok, $detail = "") {
    if ($ok) { $script:pass++; Write-Host ("PASS  " + $name) -ForegroundColor Green }
    else { $script:fail++; Write-Host ("FAIL  " + $name + "  " + $detail) -ForegroundColor Red }
}
function Ask($key, $messages, $extra = @{}) {
    $body = @{ model = "openai/gpt-4o-mini"; messages = $messages } + $extra | ConvertTo-Json -Depth 8
    $h = @{ "x-bf-cache-key" = $key }
    $r = Invoke-WebRequest -Uri "$Companion/v1/chat/completions" -Method Post -Body $body -ContentType "application/json" -Headers $h -UseBasicParsing -TimeoutSec 60
    $j = $r.Content | ConvertFrom-Json
    [pscustomobject]@{ Headers = $r.Headers; Answer = $j.choices[0].message.content }
}
function Hdr($r, $name) { $v = $r.Headers[$name]; if ($v -is [array]) { $v[0] } else { $v } }
function User($text) { ,@(@{ role = "user"; content = $text }) }

$run = [int][double]::Parse((Get-Date -UFormat %s))
Write-Host "== Services"
foreach ($u in @("$Companion/healthz", "$Bifrost/health")) {
    try { $c = (Invoke-WebRequest $u -UseBasicParsing -TimeoutSec 10).StatusCode } catch { $c = 0 }
    Check "up: $u" ($c -eq 200) "status $c"
}

Write-Host "== Cache basics (miss, then exact hit)"
$k = "smoke-basic-$run"; $q = User "Tell me one fun fact about owls ($run)."
$a = Ask $k $q; Check "first ask is a miss" ((Hdr $a "x-bifrost-cache-hit") -eq "false")
Start-Sleep -Seconds 1
$b = Ask $k $q; Check "second ask is a direct hit" ((Hdr $b "x-bifrost-cache-hit") -eq "true" -and (Hdr $b "x-bifrost-cache-hit-type") -eq "direct")
Check "hit returns the same answer" ($a.Answer -eq $b.Answer)

Write-Host "== Look-alike questions (check before serving)"
$k = "smoke-lookalike-$run"
$null = Ask $k (User "How do I reset my Acme account password from the mobile app?"); Start-Sleep -Seconds 1
$u = Ask $k (User "How do I reset my Acme account username from the mobile app?")
Check "look-alike is blocked before serving" ((Hdr $u "x-freshness-gate") -eq "blocked") "gate=$(Hdr $u 'x-freshness-gate')"
Check "and the answer is about the username" ($u.Answer -match "username")
Start-Sleep -Seconds 1
$u2 = Ask $k (User "How do I reset my Acme account username from the mobile app?")
Check "the username question then gets its own exact entry" ((Hdr $u2 "x-bifrost-cache-hit-type") -eq "direct")

Write-Host "== Cache guard"
$tool = @(@{ role = "user"; content = "What is the status of my order ($run)?" }, @{ role = "tool"; tool_call_id = "c1"; content = '{"status":"shipped"}' })
$t = Ask "smoke-tool-$run" $tool
Check "tool-result request is kept out of the cache" ((Hdr $t "x-freshness-guard") -match "tool_results")
$s = Ask "smoke-pii-$run" (User "My email is smoke.test@example.com, summarise my plan ($run)")
Check "text with an email is kept out of the cache" ((Hdr $s "x-freshness-guard") -match "sensitive:email")
$a1 = Ask "smoke-act-$run" (User "Open a ticket ($run)") @{ tools = @(@{ type = "function"; function = @{ name = "create_ticket" } }) }
Check "action tool is kept out of the cache" ((Hdr $a1 "x-freshness-guard") -match "side_effect_tool")

Write-Host "== Verified answers"
$v = Ask "smoke-verified-$run" (User "what are the support HOURS for acme?")
Check "approved answer is served with no model call" ((Hdr $v "x-freshness-verified") -eq "human" -and (Hdr $v "x-freshness-answer-source") -eq "verified")

Write-Host "== Why did I get this answer?"
$rid = Hdr $b "x-freshness-request-id"
Check "responses carry a request id" ($rid -match "^req-[0-9a-f]{8}-\d+$") "id=$rid"
$e = Invoke-RestMethod "$Companion/freshness/explain/$rid"
Check "explain says direct hit, in plain words" ($e.how -eq "direct_hit" -and $e.plain.Length -gt 20)
$list = (Invoke-WebRequest "$Companion/freshness/explain?limit=300" -UseBasicParsing).Content
Check "explain output holds no question text" (-not ($list -match "owls" -or $list -match "smoke.test@example.com"))

Write-Host "== Counters and audits"
$au = Invoke-RestMethod "$Companion/freshness/audits"
Check "audit endpoint answers with gate counters" ($null -ne $au.stats.gate_blocked)
$g = Invoke-RestMethod "$Companion/freshness/guard"
Check "guard endpoint counts the bypasses" ($g.bypassed -ge 3)
$d = Invoke-RestMethod "$Companion/freshness/decisions?limit=5"
Check "decision log endpoint answers" ($null -ne $d)

Write-Host ""
Write-Host ("{0} passed, {1} failed" -f $script:pass, $script:fail)
if ($script:fail -gt 0) { exit 1 }
