# Real-upstream protocol compatibility check.
#
# Drives Chat Completions, Responses and Anthropic Messages (stream and
# non-stream, tool calls, count_tokens, pinned route, error shape) through a
# running hub that has a real account imported. Uses the panel API only to
# enable the Anthropic switch and to find the account uid for the pinned
# route; every protocol request goes through the gateway with the API key.
#
#   pwsh tools/livetest/compat.ps1 -ApiKey sk-... -PanelPassword ...
param(
  [string]$BaseUrl = 'http://127.0.0.1:18888',
  [Parameter(Mandatory = $true)][string]$ApiKey,
  [string]$Model = 'glm-5.3-flash',
  [string]$PanelPassword = ''
)
$ErrorActionPreference = 'Stop'
$auth = @{ Authorization = "Bearer $ApiKey" }
$script:fails = 0

function Call($method, $path, $body, $headers, $timeout = 180) {
  $h = @{}; foreach ($k in $auth.Keys) { $h[$k] = $auth[$k] }
  if ($headers) { foreach ($k in $headers.Keys) { $h[$k] = $headers[$k] } }
  $p = @{ Uri = "$BaseUrl$path"; Method = $method; Headers = $h; TimeoutSec = $timeout; UseBasicParsing = $true }
  if ($body -ne $null) { $p['ContentType'] = 'application/json'; $p['Body'] = ($body | ConvertTo-Json -Depth 10 -Compress) }
  try { $r = Invoke-WebRequest @p; return @{ code = $r.StatusCode; body = $r.Content } }
  catch { return @{ code = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }; body = $_.ErrorDetails.Message } }
}

function Check($name, $cond, $detail) {
  $state = if ($cond) { 'PASS' } else { 'FAIL'; $script:fails++ }
  "{0,-30} {1}  {2}" -f $name, $state, $detail
}

# Panel session: enable Anthropic and learn the account uid.
$uid = ''
if ($PanelPassword) {
  $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
  $login = Invoke-RestMethod "$BaseUrl/api/v2/session/login" -Method Post -WebSession $sess -ContentType 'application/json' -Body (@{ password = $PanelPassword; remember = $false } | ConvertTo-Json -Compress)
  $J = @{ 'X-WB-CSRF' = $login.csrf; 'Content-Type' = 'application/json' }
  $null = Invoke-RestMethod "$BaseUrl/api/v2/settings" -Method Patch -WebSession $sess -Headers $J -Body '{"switches":{"enable_anthropic_messages":true}}'
  $page = Invoke-RestMethod "$BaseUrl/api/v2/accounts?limit=1" -WebSession $sess
  $uid = $page.items[0].uid
}

$chatNon = Call 'POST' '/v1/chat/completions' @{ model = $Model; stream = $false; max_tokens = 64; messages = @(@{ role = 'user'; content = 'Reply with exactly: pong' }) }
$j = $null; try { $j = $chatNon.body | ConvertFrom-Json } catch {}
Check 'chat/non-stream' ($chatNon.code -eq 200 -and $j.choices[0].message.content -match 'pong') "code=$($chatNon.code) content=$($j.choices[0].message.content)"

$chatStream = Call 'POST' '/v1/chat/completions' @{ model = $Model; stream = $true; max_tokens = 64; messages = @(@{ role = 'user'; content = 'Reply with exactly: pong' }) }
$deltas = [regex]::Matches($chatStream.body, '"content":"([^"\\]*(?:\\.[^"\\]*)*)"') | ForEach-Object { $_.Groups[1].Value }
Check 'chat/stream' ($chatStream.code -eq 200 -and ($deltas -join '') -match 'pong' -and $chatStream.body.Contains('[DONE]')) "code=$($chatStream.code) text=$(($deltas -join ''))"

$respNon = Call 'POST' '/v1/responses' @{ model = $Model; input = 'Reply with exactly: pong'; max_output_tokens = 64 }
$j = $null; try { $j = $respNon.body | ConvertFrom-Json } catch {}
Check 'responses/non-stream' ($respNon.code -eq 200 -and $j.output_text -match 'pong') "code=$($respNon.code) text=$($j.output_text)"

$respStream = Call 'POST' '/v1/responses' @{ model = $Model; input = 'Reply with exactly: pong'; max_output_tokens = 64; stream = $true }
$completed = $respStream.body.Contains('response.completed')
$textParts = [regex]::Matches($respStream.body, '"delta":\s*"([^"]*)"') | ForEach-Object { $_.Groups[1].Value }
Check 'responses/stream' ($respStream.code -eq 200 -and $completed -and ($textParts -join '') -match 'pong') "code=$($respStream.code) completed=$completed"

$ha = @{ 'x-api-key' = $ApiKey; 'anthropic-version' = '2023-06-01' }
$anNon = Call 'POST' '/v1/messages' @{ model = $Model; max_tokens = 64; messages = @(@{ role = 'user'; content = 'Reply with exactly: pong' }) } $ha
$j = $null; try { $j = $anNon.body | ConvertFrom-Json } catch {}
$anText = ($j.content | ForEach-Object { $_.text }) -join ''
Check 'anthropic/non-stream' ($anNon.code -eq 200 -and $anText -match 'pong') "code=$($anNon.code) text=$anText"

$anStream = Call 'POST' '/v1/messages' @{ model = $Model; max_tokens = 64; stream = $true; messages = @(@{ role = 'user'; content = 'Reply with exactly: pong' }) } $ha
$anParts = [regex]::Matches($anStream.body, '"text":"([^"]*)"') | ForEach-Object { $_.Groups[1].Value }
Check 'anthropic/stream' ($anStream.code -eq 200 -and $anStream.body.Contains('message_stop') -and ($anParts -join '') -match 'pong') "code=$($anStream.code) events=$(([regex]::Matches($anStream.body, 'event: ([a-z_]+)') | ForEach-Object { $_.Groups[1].Value } | Select-Object -Unique | Sort-Object) -join ',')"

$count = Call 'POST' '/v1/messages/count_tokens' @{ model = $Model; messages = @(@{ role = 'user'; content = 'hello world' }) } $ha 60
$j = $null; try { $j = $count.body | ConvertFrom-Json } catch {}
Check 'anthropic/count_tokens' ($count.code -eq 200 -and $j.input_tokens -ge 1) "code=$($count.code) input_tokens=$($j.input_tokens)"

if ($uid) {
  $pin = Call 'POST' "/u/$uid/v1/chat/completions" @{ model = $Model; stream = $false; max_tokens = 32; messages = @(@{ role = 'user'; content = 'Reply with exactly: pong' }) }
  Check 'pinned/chat' ($pin.code -eq 200) "code=$($pin.code)"
}

$tool = @{ type = 'function'; function = @{ name = 'get_time'; description = 'Get the current time'; parameters = @{ type = 'object'; properties = @{ tz = @{ type = 'string' } }; required = @('tz') } } }
$chatTool = Call 'POST' '/v1/chat/completions' @{ model = $Model; stream = $false; max_tokens = 128; tools = @($tool); tool_choice = 'auto'; messages = @(@{ role = 'user'; content = 'Call get_time with tz=UTC, then stop.' }) }
$j = $null; try { $j = $chatTool.body | ConvertFrom-Json } catch {}
Check 'chat/tool-call' ($chatTool.code -eq 200 -and @($j.choices[0].message.tool_calls).Count -ge 1) "code=$($chatTool.code) tool_calls=$(@($j.choices[0].message.tool_calls).Count)"

$rtool = @{ type = 'function'; name = 'get_time'; description = 'Get the current time'; parameters = @{ type = 'object'; properties = @{ tz = @{ type = 'string' } }; required = @('tz') } }
$respTool = Call 'POST' '/v1/responses' @{ model = $Model; input = 'Call get_time with tz=UTC, then stop.'; max_output_tokens = 128; tools = @($rtool) }
$j = $null; try { $j = $respTool.body | ConvertFrom-Json } catch {}
$types = ($j.output | ForEach-Object { $_.type }) -join ','
Check 'responses/tool-call' ($respTool.code -eq 200 -and $types -match 'function_call') "code=$($respTool.code) output=$types"

$atool = @{ name = 'get_time'; description = 'Get the current time'; input_schema = @{ type = 'object'; properties = @{ tz = @{ type = 'string' } }; required = @('tz') } }
$anTool = Call 'POST' '/v1/messages' @{ model = $Model; max_tokens = 128; tools = @($atool); messages = @(@{ role = 'user'; content = 'Call get_time with tz=UTC, then stop.' }) } $ha
$j = $null; try { $j = $anTool.body | ConvertFrom-Json } catch {}
$blocks = ($j.content | ForEach-Object { $_.type }) -join ','
Check 'anthropic/tool-call' ($anTool.code -eq 200 -and $blocks -match 'tool_use') "code=$($anTool.code) blocks=$blocks stop=$($j.stop_reason)"

$bad = Call 'POST' '/v1/chat/completions' @{ model = 'no-such-model'; stream = $false; max_tokens = 8; messages = @(@{ role = 'user'; content = 'hi' }) }
Check 'chat/unknown-model' ($bad.code -ge 400 -and $bad.code -lt 500) "code=$($bad.code)"

$models = Call 'GET' '/v1/models?realm=cn' $null $null 60
$j = $null; try { $j = $models.body | ConvertFrom-Json } catch {}
Check 'models/realm=cn' ($models.code -eq 200 -and $j.data.Count -ge 1) "code=$($models.code) count=$($j.data.Count)"

if ($script:fails -gt 0) { "FAILED: $($script:fails) check(s)"; exit 1 }
"all protocol checks passed"
