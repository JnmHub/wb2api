# Many-accounts scheduling test.
#
# Builds pools of 10 / 100 / 1000 accounts (plus a 1000-account pool where 90%
# are disabled) and checks that request throughput, first-token latency and the
# panel APIs do not degrade as the pool grows. The point is the request path and
# account pick: the unattended chore cycle is intentionally staggered over
# hours and is not part of this measurement.
#
#   pwsh tools/bench/many-accounts.ps1
#   pwsh tools/bench/many-accounts.ps1 -Sizes 100,1000 -Concurrency 100
param(
  [int[]]$Sizes = @(10, 100, 1000),
  [int]$Concurrency = 50,
  [int]$Requests = 200,
  [int]$BasePort = 18892,
  [int]$BaseMockPort = 19002,
  [switch]$SkipDisabledVariant
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Push-Location $root
$report = 'run\many\report.txt'
try {
  if (!(Test-Path 'bin\hub.exe') -or !(Test-Path 'bin\mockup.exe') -or !(Test-Path 'bin\loadtest.exe')) {
    go build -o bin/ ./cmd/...
  }
  if (Test-Path 'run\many') { Remove-Item -Recurse -Force 'run\many' }
  New-Item -ItemType Directory -Force 'run\many' | Out-Null
  "many-accounts test $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')" | Set-Content $report -Encoding UTF8

  $key = 'sk-many'
  $password = 'bench-pass'
  $scenarios = @()
  foreach ($n in $Sizes) { $scenarios += [pscustomobject]@{ Name = "$n accounts"; N = $n; Disable = 0.0 } }
  if (!$SkipDisabledVariant -and ($Sizes -contains 1000)) {
    $scenarios += [pscustomobject]@{ Name = '1000 accounts, 90% disabled'; N = 1000; Disable = 0.9 }
  }

  $idx = 0
  foreach ($sc in $scenarios) {
    $hubPort = $BasePort + $idx
    $mockPort = $BaseMockPort + $idx
    $idx++
    $dir = "run\many\s$idx"
    New-Item -ItemType Directory -Force $dir | Out-Null
    & bin\loadtest.exe genaccounts -dir "$dir\accounts" -n $sc.N -key $key | Out-Null
    if ($sc.Disable -gt 0) {
      $cut = [int][math]::Floor($sc.N * $sc.Disable)
      $files = Get-ChildItem "$dir\accounts" -Filter '*.json' | Where-Object { $_.BaseName -ne 'settings' } | Select-Object -First $cut
      foreach ($f in $files) {
        $doc = Get-Content $f.FullName -Raw | ConvertFrom-Json
        $doc.enabled = $false
        $doc | ConvertTo-Json -Depth 5 | Set-Content $f.FullName -Encoding UTF8
      }
    }

    $mock = Start-Process -FilePath 'bin\mockup.exe' -ArgumentList @('-addr', "127.0.0.1:$mockPort", '-tokens', '200', '-tps', '50') -PassThru -WindowStyle Hidden
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $hub = Start-Process -FilePath 'bin\hub.exe' -ArgumentList @(
      '-port', "$hubPort", '-accounts-dir', "$dir\accounts", '-usage-dir', "$dir\usage",
      '-catalog-dir', "$dir\catalog", '-upstream-override', "http://127.0.0.1:$mockPort", '-no-modelsdev',
      '-panel-password', $password, '-debug'
    ) -PassThru -WindowStyle Hidden
    try {
      $healthy = $false
      for ($i = 0; $i -lt 240; $i++) {
        Start-Sleep -Milliseconds 250
        try { $null = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$hubPort/health" -TimeoutSec 2; $healthy = $true; break } catch {}
      }
      if (!$healthy) { throw "hub not healthy for $($sc.Name)" }
      $startupMs = $watch.ElapsedMilliseconds

      # Panel API timings (first page, filtered/sorted page, summary).
      $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
      $login = Invoke-RestMethod "http://127.0.0.1:$hubPort/api/v2/session/login" -Method Post -WebSession $sess -ContentType 'application/json' -Body '{"password":"bench-pass","remember":false}'
      $J = @{ 'X-WB-CSRF' = $login.csrf }
      function TimeMs([string]$path, [int]$n = 20) {
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        for ($i = 0; $i -lt $n; $i++) { $null = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$hubPort$path" -WebSession $sess -Headers $J -TimeoutSec 30 }
        return [math]::Round($sw.Elapsed.TotalMilliseconds / $n, 2)
      }
      $tPage = TimeMs '/api/v2/accounts?limit=100'
      $tFiltered = TimeMs '/api/v2/accounts?status=ready&sort=name&order=asc&limit=50'
      $tSummary = TimeMs '/api/v2/accounts/summary'
      $tUsage = TimeMs '/api/v2/usage/summary?range=today'

      $runtime = Invoke-RestMethod "http://127.0.0.1:$hubPort/debug/runtime"
      $heapMB = [math]::Round($runtime.heap_alloc / 1MB, 1)

      $out = & bin\loadtest.exe -url "http://127.0.0.1:$hubPort/v1/chat/completions" -key $key -model 'glm-5.3' `
        -tokens 200 -tps 50 -max 200 -c $Concurrency -n $Requests 2>&1 | Out-String
      $ok = ([regex]::Match($out, 'requests\s+:\s+(\d+) ok / (\d+) total in ([\d.]+m?s)')).Groups
      $rpm = ([regex]::Match($out, 'RPM\s+:\s+(\d+)')).Groups[1].Value
      $tps = ([regex]::Match($out, 'TPS\s+:\s+([\d.]+)')).Groups[1].Value
      $ttft = ([regex]::Match($out, 'TTFT\s+:\s+p50 (\S+)\s+p99 (\S+)')).Groups
      $errors = ([regex]::Matches($out, 'error x\d+')).Count

      $line = "{0,-28} start={1,6}ms  {2}/{3} ok  RPM={4,5}  TPS={5,7}  TTFT p50={6,-9} p99={7,-9}  err={8}  heap={9}MB  api: page={10}ms filtered={11}ms summary={12}ms usage={13}ms" -f `
        $sc.Name, $startupMs, $ok[1].Value, $ok[2].Value, $rpm, $tps, $ttft[1].Value, $ttft[2].Value, $errors, $heapMB, $tPage, $tFiltered, $tSummary, $tUsage
      $line | Tee-Object -FilePath $report -Append
      "--- loadtest output for $($sc.Name) ---" | Add-Content $report -Encoding UTF8
      $out | Add-Content $report -Encoding UTF8
    } finally {
      if ($hub) { Stop-Process -Id $hub.Id -Force -ErrorAction SilentlyContinue }
      if ($mock) { Stop-Process -Id $mock.Id -Force -ErrorAction SilentlyContinue }
    }
  }
  "`nreport: $report"
} finally {
  Pop-Location
}
