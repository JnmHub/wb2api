# Real-account feature sweep: import is expected to have happened already.
# Exercises credits, the account probe, CN check-in, activity report, the
# growth snapshot, cat travel, the streak keeper, and optionally the full
# growth auto-run. Destructive/browser actions are not included.
#
#   pwsh tools/livetest/account-ops.ps1 -PanelPassword ... -Model hy3
param(
  [string]$BaseUrl = 'http://127.0.0.1:18888',
  [Parameter(Mandatory = $true)][string]$PanelPassword,
  [string]$Model = 'hy3',
  [switch]$FullGrowth
)
$ErrorActionPreference = 'Stop'
$sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
$login = Invoke-RestMethod "$BaseUrl/api/v2/session/login" -Method Post -WebSession $sess -ContentType 'application/json' -Body (@{ password = $PanelPassword; remember = $false } | ConvertTo-Json -Compress)
$J = @{ 'X-WB-CSRF' = $login.csrf; 'Content-Type' = 'application/json' }

function PostJson($path, $obj, $timeout = 300) {
  $body = if ($obj -eq $null) { '{}' } else { $obj | ConvertTo-Json -Depth 6 -Compress }
  try { return Invoke-RestMethod "$BaseUrl$path" -Method Post -WebSession $sess -Headers $J -Body $body -TimeoutSec $timeout }
  catch { return @{ error = $_.ErrorDetails.Message } }
}

$page = Invoke-RestMethod "$BaseUrl/api/v2/accounts?limit=10" -WebSession $sess
$acc = $page.items | Where-Object { $_.realm -eq 'cn' } | Select-Object -First 1
if (!$acc) { throw 'no CN account found' }
$uid = $acc.uid
"account: $($acc.nickname) realm=$($acc.realm) status=$($acc.status) expires=$($acc.expires_at)"

$credits = PostJson "/api/v2/accounts/$uid/actions/credits" $null
"credits      ok=$($credits.ok) error=$($credits.error)"

$probe = PostJson "/api/v2/accounts/$uid/actions/test" @{ model = $Model } 180
"probe        ok=$($probe.ok) elapsed_ms=$($probe.elapsed_ms) status=$($probe.status) reply=$($probe.reply) error=$($probe.error)"

$checkin = PostJson "/api/v2/accounts/$uid/actions/checkin" $null
"checkin      ok=$($checkin.ok) msg=$($checkin.msg) error=$($checkin.error)"

$report = PostJson "/api/v2/accounts/$uid/actions/report-activity" $null
"report       ok=$($report.ok) msg=$($report.msg) error=$($report.error)"

$tasks = Invoke-RestMethod "$BaseUrl/api/v2/tasks" -WebSession $sess
"tasks        count=$($tasks.tasks.Count) streak=$($tasks.summary.streak_days) energy=$($tasks.summary.energy) travel=$($tasks.summary.travel.state)"

$travel = PostJson "/api/v2/tasks/travel" @{} 300
"travel       ok=$($travel.ok) msg=$($travel.msg)"

$streak = PostJson "/api/v2/tasks/streak-bonus" @{} 300
"streak       ok=$($streak.ok) msg=$($streak.msg)"

if ($FullGrowth) {
  $run = PostJson "/api/v2/tasks/run" @{} 900
  "growth-run   ok=$($run.ok) credit_added=$($run.credit_added) logs=$($run.logs.Count)"
}

$after = Invoke-RestMethod "$BaseUrl/api/v2/accounts/$uid" -WebSession $sess
"after        credits.remain=$($after.credits.remain)"
