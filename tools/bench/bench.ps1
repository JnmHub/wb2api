# Mock concurrency benchmark for the gateway.
#
# Starts the mock upstream and a second hub pointed at it, generates a fake
# account pool, then drives the gateway through cmd/loadtest at increasing
# concurrency. Writes the raw report to run/bench/bench.txt.
#
#   pwsh tools/bench/bench.ps1
#   pwsh tools/bench/bench.ps1 -ChatConcurrency 10,50,200 -ProtocolConcurrency 100
param(
  [int]$Port = 18889,
  [int]$MockPort = 19000,
  [string]$Key = 'sk-bench',
  [int[]]$ChatConcurrency = @(10, 50, 100, 200),
  [int]$ProtocolConcurrency = 50,
  [int]$Tokens = 200,
  [int]$MockTPS = 50
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Push-Location $root
$mock = $null
$hub = $null
try {
  if (!(Test-Path 'bin\hub.exe') -or !(Test-Path 'bin\mockup.exe') -or !(Test-Path 'bin\loadtest.exe')) {
    go build -o bin/ ./cmd/...
  }
  if (Test-Path 'run\bench') { Remove-Item -Recurse -Force 'run\bench' }
  New-Item -ItemType Directory -Force 'run\bench' | Out-Null
  & bin\loadtest.exe genaccounts -dir run\bench\accounts -n 50 -key $Key | Out-Null

  $mock = Start-Process -FilePath 'bin\mockup.exe' -ArgumentList @('-addr', "127.0.0.1:$MockPort", '-tokens', "$Tokens", '-tps', "$MockTPS") -PassThru -WindowStyle Hidden
  $hub = Start-Process -FilePath 'bin\hub.exe' -ArgumentList @(
    '-port', "$Port", '-accounts-dir', 'run\bench\accounts', '-usage-dir', 'run\bench\usage',
    '-catalog-dir', 'run\bench\catalog', '-upstream-override', "http://127.0.0.1:$MockPort", '-no-modelsdev', '-debug'
  ) -PassThru -WindowStyle Hidden

  $ok = $false
  for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Milliseconds 250
    try { $null = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$Port/health" -TimeoutSec 2; $ok = $true; break } catch {}
  }
  if (!$ok) { throw "hub did not become healthy on port $Port" }

  # Anthropic Messages is opt-in.
  $sp = 'run\bench\accounts\settings.json'
  $cfg = Get-Content $sp -Raw | ConvertFrom-Json
  $cfg | Add-Member -NotePropertyName enable_anthropic_messages -NotePropertyValue $true -Force
  $cfg | ConvertTo-Json -Depth 6 | Set-Content $sp -Encoding UTF8
  Start-Sleep -Milliseconds 700

  $log = 'run\bench\bench.txt'
  "gateway benchmark $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')" | Set-Content $log -Encoding UTF8
  "hub=:$Port mock=:$MockPort tokens=$Tokens mock_tps=$MockTPS accounts=50" | Add-Content $log -Encoding UTF8

  function Bench([string]$label, [string[]]$extra) {
    $line = "`n===== $label ====="
    $line | Tee-Object -FilePath $log -Append
    $lt = @('-url', "http://127.0.0.1:$Port/v1/chat/completions", '-key', $Key, '-model',
      'glm-5.3', '-tokens', "$Tokens", '-tps', "$MockTPS", '-max', "$Tokens") + $extra
    & bin\loadtest.exe @lt 2>&1 | Tee-Object -FilePath $log -Append
  }

  foreach ($c in $ChatConcurrency) {
    Bench "chat c=$c n=$($c*4)" @('-c', "$c", '-n', "$($c*4)")
  }
  Bench "responses c=$ProtocolConcurrency n=$($ProtocolConcurrency*4)" @('-c', "$ProtocolConcurrency", '-n', "$($ProtocolConcurrency*4)", '-mode', 'responses')
  Bench "anthropic c=$ProtocolConcurrency n=$($ProtocolConcurrency*4)" @('-c', "$ProtocolConcurrency", '-n', "$($ProtocolConcurrency*4)", '-mode', 'anthropic')
  "`nreport: $log"
} finally {
  if ($hub) { Stop-Process -Id $hub.Id -Force -ErrorAction SilentlyContinue }
  if ($mock) { Stop-Process -Id $mock.Id -Force -ErrorAction SilentlyContinue }
  Pop-Location
}
