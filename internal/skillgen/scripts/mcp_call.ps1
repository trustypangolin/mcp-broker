<#
.SYNOPSIS
mcp_call: call a tool on one MCP server, with nothing to install (Windows PowerShell 5.1+).

.DESCRIPTION
Written by mcp-broker for skills that reach a server directly (no broker). The server is
described by a server.json next to the skill (transport stdio: command, args, cwd, env;
transport http: url, headers). ${VAR} in it is read from the environment (and from
server.json's own env); $$ is a literal $. Secrets are never in server.json: set the
variables it names before calling.

  mcp_call.ps1 -Server server.json -List
  mcp_call.ps1 -Server server.json -Call TOOL -ArgsJson '{"name": "value"}'
  mcp_call.ps1 -Server server.json -Call TOOL -ArgsJson '@body.json'
  mcp_call.ps1 -Server server.json -Steps steps.json

Each run starts the server and stops it afterwards, so state (an open document) does not
survive between runs; put calls that share state in a steps file, a JSON list of
{"tool": "...", "arguments": {...}} run in order on one server.

Prints the server's JSON-RPC response (a list of them for -Steps); the tool's answer is
under "result" (result.content[].text; result.isError true means the tool failed). Exit
status: 0 ok, 2 a tool reported an error, 1 anything else (message on stderr).
#>
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [switch]$List,
    [string]$Call,
    [string]$ArgsJson = '{}',
    [string]$Steps,
    [switch]$KeepGoing,
    [int]$Timeout = 120
)
$ErrorActionPreference = 'Stop'
$Utf8 = New-Object System.Text.UTF8Encoding $false
try { [Console]::OutputEncoding = $Utf8 } catch { }
$Protocol = '2025-06-18'

function Fail([string]$msg) {
    [Console]::Error.WriteLine("mcp_call: $msg")
    exit 1
}

if ((@($List.IsPresent, [bool]$Call, [bool]$Steps) | Where-Object { $_ }).Count -ne 1) {
    Fail 'give exactly one of -List, -Call TOOL or -Steps FILE'
}

# ---- server.json and ${VAR} expansion ----

$script:missing = New-Object 'System.Collections.Generic.SortedSet[string]'
$envVars = New-Object 'System.Collections.Hashtable' ([StringComparer]::OrdinalIgnoreCase)
foreach ($e in [Environment]::GetEnvironmentVariables().GetEnumerator()) { $envVars[$e.Key] = $e.Value }

function Expand([string]$s) {
    if ($null -eq $s) { return '' }
    $b = New-Object System.Text.StringBuilder
    $i = 0
    while ($i -lt $s.Length) {
        $c = $s[$i]
        if ($c -eq '$' -and $i + 1 -lt $s.Length) {
            $n = $s[$i + 1]
            if ($n -eq '$') { [void]$b.Append('$'); $i += 2; continue }
            if ($n -eq '{') {
                $j = $s.IndexOf('}', $i + 2)
                if ($j -lt 0) { Fail "unclosed `${ in '$s'" }
                $name = $s.Substring($i + 2, $j - $i - 2)
                if ($envVars.ContainsKey($name)) { [void]$b.Append($envVars[$name]) } else { [void]$script:missing.Add($name) }
                $i = $j + 1
                continue
            }
        }
        [void]$b.Append($c)
        $i++
    }
    return $b.ToString()
}

if (-not (Test-Path -LiteralPath $Server)) { Fail "no such file: $Server" }
$srv = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $Server).Path, $Utf8) | ConvertFrom-Json
$ownEnv = @{}
if ($srv.env) { foreach ($p in $srv.env.PSObject.Properties) { $ownEnv[$p.Name] = Expand ([string]$p.Value) } }
foreach ($k in $ownEnv.Keys) { $envVars[$k] = $ownEnv[$k] }
$command = Expand $srv.command
$cmdArgs = @(); foreach ($a in @($srv.args)) { if ($null -ne $a) { $cmdArgs += (Expand ([string]$a)) } }
$cwd = Expand $srv.cwd
$url = Expand $srv.url
$headers = @{}
if ($srv.headers) { foreach ($p in $srv.headers.PSObject.Properties) { $headers[$p.Name] = Expand ([string]$p.Value) } }
if ($script:missing.Count -gt 0) {
    Fail ('set these environment variables first (server.json uses them): ' + ($script:missing -join ', '))
}

# ---- arguments ----

function Read-JsonArg([string]$text, [string]$what) {
    if ($text.StartsWith('@')) {
        $f = $text.Substring(1)
        if (-not (Test-Path -LiteralPath $f)) { Fail "$what file not found: $f" }
        $text = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $f).Path, $Utf8)
    }
    try { [void]($text | ConvertFrom-Json) } catch { Fail "$what is not valid JSON: $($_.Exception.Message)" }
    return $text.Trim()
}

function JsonString([string]$s) { return (ConvertTo-Json -InputObject $s -Compress) }

$plan = @()
if ($Call) {
    $plan += , @($Call, (Read-JsonArg $ArgsJson '-ArgsJson'))
}
elseif ($Steps) {
    $parsed = Read-JsonArg ('@' + $Steps) '-Steps' | ConvertFrom-Json
    foreach ($s in @($parsed)) {
        if (-not $s.tool) { Fail '-Steps must be a JSON list of {"tool": "...", "arguments": {...}}' }
        $argText = '{}'
        if ($null -ne $s.arguments) { $argText = ConvertTo-Json -InputObject $s.arguments -Depth 50 -Compress }
        $plan += , @([string]$s.tool, $argText)
    }
}

# ---- replies to requests the server sends us ----

function Answer($m) {
    $id = ConvertTo-Json -InputObject $m.id -Compress
    switch ([string]$m.method) {
        'ping' { return '{"jsonrpc":"2.0","id":' + $id + ',"result":{}}' }
        'roots/list' { return '{"jsonrpc":"2.0","id":' + $id + ',"result":{"roots":[]}}' }
        'elicitation/create' { return '{"jsonrpc":"2.0","id":' + $id + ',"result":{"action":"decline"}}' }
        default { return '{"jsonrpc":"2.0","id":' + $id + ',"error":{"code":-32601,"message":"not supported by mcp_call"}}' }
    }
}

function Has($o, [string]$name) { return $null -ne $o -and $null -ne $o.PSObject.Properties[$name] }

# ---- stdio transport ----

function Quote-Arg([string]$a) {
    if ($a -eq '') { return '""' }
    if ($a -notmatch '[\s"]') { return $a }
    $q = $a -replace '(\\*)"', '$1$1\"'
    $q = $q -replace '(\\+)$', '$1$1'
    return '"' + $q + '"'
}

function Start-Stdio {
    $exe = $command
    $gc = Get-Command $command -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($gc) { $exe = $gc.Source }
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = (($cmdArgs | ForEach-Object { Quote-Arg $_ }) -join ' ')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = $Utf8
    $psi.StandardErrorEncoding = $Utf8
    $psi.CreateNoWindow = $true
    if ($cwd) { $psi.WorkingDirectory = $cwd }
    foreach ($k in $ownEnv.Keys) { $psi.EnvironmentVariables[$k] = $ownEnv[$k] }
    # .NET Framework gives the child's stdin a writer in the console's input encoding, and a
    # UTF-8 one writes a byte-order mark the moment it is created: the server would read
    # "﻿{..." and reject the first message. Start it with a BOM-less encoding.
    try { [Console]::InputEncoding = $Utf8 } catch { }
    try { $script:proc = [System.Diagnostics.Process]::Start($psi) }
    catch { Fail "cannot start $command`: $($_.Exception.Message) (is it installed here? fix the path in server.json)" }
    $script:errTask = $script:proc.StandardError.ReadToEndAsync()
    $script:stdin = $script:proc.StandardInput.BaseStream
}

function Died {
    $err = ''
    if ($script:errTask.Wait(2000)) { $err = ($script:errTask.Result -split "`n" | Select-Object -Last 20) -join "`n  " }
    Fail "the server exited; its last output:`n  $err"
}

function Send-Stdio([string]$line) {
    $b = $Utf8.GetBytes($line + "`n")
    try { $script:stdin.Write($b, 0, $b.Length); $script:stdin.Flush() } catch { Died }
}

function Request-Stdio([string]$json, [int]$id) {
    Send-Stdio $json
    while ($true) {
        $t = $script:proc.StandardOutput.ReadLineAsync()
        if (-not $t.Wait($Timeout * 1000)) { Fail "no answer from the server within $Timeout s" }
        $line = $t.Result
        if ($null -eq $line) { Died }
        try { $m = $line | ConvertFrom-Json } catch { continue }
        if ((Has $m 'method') -and (Has $m 'id')) { Send-Stdio (Answer $m); continue }
        if ((Has $m 'id') -and $m.id -eq $id) { return , @($line, $m) }
        # An error with no id answers a message the server couldn't read: no reply to ours
        # will follow, so waiting would only time out.
        if ((Has $m 'error') -and $null -eq $m.id) { Fail ('the server could not read the request: ' + (ConvertTo-Json -InputObject $m.error -Compress)) }
    }
}

function Close-Stdio {
    try { $script:stdin.Close() } catch { }
    if (-not $script:proc.WaitForExit(5000)) { try { $script:proc.Kill() } catch { } }
}

# ---- HTTP transport ----

$script:sid = $null
$script:version = $null
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Post-Http([string]$json) {
    $req = [System.Net.HttpWebRequest]::Create($url)
    $req.Method = 'POST'
    $req.ContentType = 'application/json'
    $req.Accept = 'application/json, text/event-stream'
    $req.Timeout = $Timeout * 1000
    $req.ReadWriteTimeout = $Timeout * 1000
    foreach ($k in $headers.Keys) { $req.Headers[$k] = $headers[$k] }
    if ($script:sid) { $req.Headers['Mcp-Session-Id'] = $script:sid }
    if ($script:version) { $req.Headers['MCP-Protocol-Version'] = $script:version }
    $b = $Utf8.GetBytes($json)
    $req.ContentLength = $b.Length
    try {
        $s = $req.GetRequestStream(); $s.Write($b, 0, $b.Length); $s.Close()
        $resp = $req.GetResponse()
    }
    catch [System.Net.WebException] {
        $r = $_.Exception.Response
        if ($null -eq $r) { Fail "cannot reach $url`: $($_.Exception.Message)" }
        $code = [int]$r.StatusCode
        $body = (New-Object IO.StreamReader($r.GetResponseStream(), $Utf8)).ReadToEnd()
        if ($body.Length -gt 500) { $body = $body.Substring(0, 500) }
        $hint = ''
        if ($code -eq 401 -or $code -eq 403) { $hint = ' (sign-in or token needed: check the header variables)' }
        Fail "HTTP $code from $url$hint`: $body"
    }
    if ($resp.Headers['Mcp-Session-Id']) { $script:sid = $resp.Headers['Mcp-Session-Id'] }
    return $resp
}

# Read-Http returns the messages in a response, stopping early at the one with id $want.
function Read-Http($resp, $want) {
    $out = @()
    if ([int]$resp.StatusCode -eq 202) { $resp.Close(); return , $out }
    $reader = New-Object IO.StreamReader($resp.GetResponseStream(), $Utf8)
    try {
        if ($resp.ContentType -like '*text/event-stream*') {
            $data = New-Object System.Collections.Generic.List[string]
            while ($true) {
                $line = $reader.ReadLine()
                if ($null -ne $line -and $line.StartsWith('data:')) { $data.Add($line.Substring(5).TrimStart()); continue }
                if (($null -eq $line -or $line -eq '') -and $data.Count -gt 0) {
                    $raw = $data -join "`n"
                    $data.Clear()
                    $out += , $raw
                    try { $m = $raw | ConvertFrom-Json } catch { $m = $null }
                    if ($null -ne $want -and (Has $m 'id') -and -not (Has $m 'method') -and $m.id -eq $want) { break }
                }
                if ($null -eq $line) { break }
            }
        }
        else {
            $body = $reader.ReadToEnd().Trim()
            if ($body.StartsWith('[')) { foreach ($x in ($body | ConvertFrom-Json)) { $out += , (ConvertTo-Json -InputObject $x -Depth 50 -Compress) } }
            elseif ($body) { $out += , $body }
        }
    }
    finally { $resp.Close() }
    return , $out
}

function Request-Http([string]$json, [int]$id) {
    foreach ($raw in (Read-Http (Post-Http $json) $id)) {
        $m = $raw | ConvertFrom-Json
        if ((Has $m 'method') -and (Has $m 'id')) { [void](Read-Http (Post-Http (Answer $m)) $null); continue }
        if ((Has $m 'id') -and $m.id -eq $id) { return , @($raw, $m) }
    }
    Fail 'the server closed the response without answering'
}

function Close-Http {
    if (-not $script:sid) { return }
    try {
        $req = [System.Net.HttpWebRequest]::Create($url)
        $req.Method = 'DELETE'
        foreach ($k in $headers.Keys) { $req.Headers[$k] = $headers[$k] }
        $req.Headers['Mcp-Session-Id'] = $script:sid
        $req.Timeout = 5000
        $req.GetResponse().Close()
    }
    catch { }
}

# ---- run ----

$http = $srv.transport -eq 'http'
if ($http) { if (-not $url) { Fail 'server.json has no url' } } else { if (-not $command) { Fail 'server.json has no command' }; Start-Stdio }

function Request([string]$json, [int]$id) { if ($http) { return Request-Http $json $id } else { return Request-Stdio $json $id } }

$status = 0
try {
    $init = Request ('{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"' + $Protocol + '","capabilities":{},"clientInfo":{"name":"mcp_call","version":"1"}}}') 1
    if (Has $init[1] 'error') { Fail ('initialize failed: ' + (ConvertTo-Json -InputObject $init[1].error -Compress)) }
    if ($http) {
        $script:version = $Protocol
        if ((Has $init[1] 'result') -and $init[1].result.protocolVersion) { $script:version = $init[1].result.protocolVersion }
        [void](Read-Http (Post-Http '{"jsonrpc":"2.0","method":"notifications/initialized"}') $null)
    }
    else { Send-Stdio '{"jsonrpc":"2.0","method":"notifications/initialized"}' }

    if ($List) {
        $tools = @(); $cursor = $null; $n = 2; $raw = $null; $pages = 0
        do {
            $params = '{}'
            if ($cursor) { $params = '{"cursor":' + (JsonString $cursor) + '}' }
            $r = Request ('{"jsonrpc":"2.0","id":' + $n + ',"method":"tools/list","params":' + $params + '}') $n
            if (Has $r[1] 'error') { Write-Output ($r[0]); $status = 1; break }
            $raw = $r[0]; $pages++
            $tools += @($r[1].result.tools)
            $cursor = $null
            if (Has $r[1].result 'nextCursor') { $cursor = $r[1].result.nextCursor }
            $n++
        } while ($cursor)
        if ($status -eq 0) {
            if ($pages -eq 1) { Write-Output ($raw) }
            else { Write-Output ((ConvertTo-Json -InputObject @{ jsonrpc = '2.0'; id = 2; result = @{ tools = $tools } } -Depth 50)) }
        }
    }
    else {
        $outs = @(); $n = 2
        foreach ($step in $plan) {
            $r = Request ('{"jsonrpc":"2.0","id":' + $n + ',"method":"tools/call","params":{"name":' + (JsonString $step[0]) + ',"arguments":' + $step[1] + '}}') $n
            $outs += , $r[0]
            $bad = 0
            if (Has $r[1] 'error') { $bad = 1 } elseif ((Has $r[1] 'result') -and $r[1].result.isError) { $bad = 2 }
            if ($status -eq 0) { $status = $bad }
            $n++
            if ($bad -and -not $KeepGoing) { break }
        }
        if ($Steps) { Write-Output ('[' + ($outs -join ",`n") + ']') } else { Write-Output ($outs[0]) }
    }
}
finally {
    if ($http) { Close-Http } else { Close-Stdio }
}
exit $status
