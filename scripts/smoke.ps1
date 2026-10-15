# Local production-image smoke test. Only mock secrets and an invalid IP source
# are supplied, so no Cloudflare calls or DNS writes are possible.
param([string]$Image = 'homelab-cloudflare-ddns:local', [string]$Platform = 'linux/amd64')
$ErrorActionPreference = 'Stop'
$projectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$fixtureRoot = [IO.Path]::GetFullPath((Join-Path $projectRoot ('dist/smoke-' + [guid]::NewGuid().ToString('N'))))
$containerName = 'ddns-smoke-' + [guid]::NewGuid().ToString('N')
$volumeName = $containerName + '-state'
$client = $null
function Docker-Checked {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    $output = & docker @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Docker failed: $($Arguments[0])" }
    return $output
}
try {
    New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
    $utf8 = [Text.UTF8Encoding]::new($false)
    [IO.File]::WriteAllText((Join-Path $fixtureRoot 'token'), 'mock-api-token', $utf8)
    [IO.File]::WriteAllText((Join-Path $fixtureRoot 'ip'), '127.0.0.1', $utf8)
    $config = @{
        zone_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
        interval = '5m'; request_timeout = '1s'; listen = ':8080'; state_dir = '/state'
        token_file = '/fixtures/token'
        ipv4 = @{ file = '/fixtures/ip' }
        records = @(@{name = 'home.example.com'; type = 'A'})
    } | ConvertTo-Json -Depth 5
    [IO.File]::WriteAllText((Join-Path $fixtureRoot 'config.json'), $config, $utf8)
    Docker-Checked @('run', '-d', '--platform', $Platform, '--name', $containerName, '--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges:true', '-p', '127.0.0.1::8080', '--mount', "type=bind,source=$fixtureRoot,target=/fixtures,readonly", '--mount', "type=volume,source=$volumeName,target=/state", '--health-cmd', '/ddns -config /fixtures/config.json -healthcheck', $Image, '-config', '/fixtures/config.json') | Out-Null
    $port = (Docker-Checked @('port',$containerName,'8080/tcp')).Trim().Split(':')[-1]
    Add-Type -AssemblyName System.Net.Http
    $client = [Net.Http.HttpClient]::new()
    $client.Timeout = [TimeSpan]::FromSeconds(5)
    $base = "http://127.0.0.1:$port"
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    do {
        try { $response = $client.GetAsync("$base/healthz").GetAwaiter().GetResult(); break } catch { Start-Sleep -Milliseconds 200 }
    } while ([DateTime]::UtcNow -lt $deadline)
    if (!$response -or [int]$response.StatusCode -ne 503) { throw 'Invalid discovery must produce unhealthy status' }
    $response.Dispose()
    $response = $client.GetAsync("$base/api/status").GetAwaiter().GetResult()
    if ([int]$response.StatusCode -ne 200 -or $response.Headers.WwwAuthenticate.Count -ne 0) { throw 'Dashboard must open without sign-in' }
    $response.Dispose()
    $html = $client.GetStringAsync($base + '/').GetAwaiter().GetResult()
    if ($html -notmatch "const csrf='([a-f0-9]+)'" ) { throw 'Dashboard CSRF token missing' }
    $csrf = $Matches[1]
    if ($html.Contains('id="ipv6"') -or $html.Contains("el('ipv6')")) { throw 'IPv6 card must be absent' }
    $response = $client.PostAsync("$base/api/force",[Net.Http.StringContent]::new('')).GetAwaiter().GetResult()
    if ([int]$response.StatusCode -ne 403) { throw 'Force action must require a CSRF token' }
    $response.Dispose()
    $status = $client.GetStringAsync("$base/api/status").GetAwaiter().GetResult() | ConvertFrom-Json
    if ($status.results[0].result -ne 'failed' -or $status.results[0].error -notmatch 'public') { throw 'Invalid IP failure missing from dashboard' }
    $client.DefaultRequestHeaders.Add('X-CSRF-Token',$csrf)
    $response = $client.PostAsync("$base/api/force",[Net.Http.StringContent]::new('')).GetAwaiter().GetResult()
    if ([int]$response.StatusCode -ne 202) { throw 'Force action not accepted' }
    $response.Dispose()
    do { Start-Sleep -Milliseconds 100; $status = $client.GetStringAsync("$base/api/status").GetAwaiter().GetResult() | ConvertFrom-Json } while ($status.running -and [DateTime]::UtcNow -lt $deadline)
    if ($status.running -or !$status.results[0].forced -or $status.history.Count -lt 2) { throw 'Force result/history missing' }
    Docker-Checked @('restart',$containerName) | Out-Null
    $port = (Docker-Checked @('port',$containerName,'8080/tcp')).Trim().Split(':')[-1]
    $base = "http://127.0.0.1:$port"
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    $restored = $false
    do {
        try { $status = $client.GetStringAsync("$base/api/status").GetAwaiter().GetResult() | ConvertFrom-Json; $restored = $true; break } catch { Start-Sleep -Milliseconds 200 }
    } while ([DateTime]::UtcNow -lt $deadline)
    if (!$restored) { throw 'Container did not become ready after restart' }
    if ($status.history.Count -lt 3) { throw 'History did not survive container restart' }
    $user = (Docker-Checked @('inspect','--format','{{.Config.User}}',$containerName)).Trim()
    if ($user -ne '65532:65532') { throw 'Container is not running as expected non-root user' }
    Write-Output 'PASS: production image, non-root/read-only runtime, direct dashboard access, CSRF force action, discovery errors, state writes and restart history. No Cloudflare calls.'
} finally {
    if ($client) { $client.Dispose() }
    & docker rm -f $containerName 2>$null | Out-Null
    & docker volume rm $volumeName 2>$null | Out-Null
    # Guard the absolute recursive-delete target before removing mock fixtures.
    $expectedParent = [IO.Path]::GetFullPath((Join-Path $projectRoot 'dist'))
    if ([IO.Path]::GetDirectoryName($fixtureRoot) -eq $expectedParent -and [IO.Path]::GetFileName($fixtureRoot).StartsWith('smoke-')) {
        if (Test-Path -LiteralPath $fixtureRoot) { Remove-Item -LiteralPath $fixtureRoot -Recurse -Force }
    }
}
