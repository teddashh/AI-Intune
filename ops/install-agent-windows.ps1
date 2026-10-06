#Requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$Step = 'initialization'

function Fail([string]$Message) {
  [Console]::Error.WriteLine("Error: $Message")
  exit 1
}

function XmlEscape([string]$Value) {
  return (($Value -replace '&', '&amp;') -replace '<', '&lt;') -replace '>', '&gt;'
}

function Set-PrivateFile([string]$Path) {
  $acl = Get-Acl -LiteralPath $Path
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($rule in @($acl.Access)) {
    $acl.RemoveAccessRule($rule) | Out-Null
  }
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
  if ($null -eq $user) {
    Fail "Current user security identifier is unavailable"
  }
  $allow = New-Object System.Security.AccessControl.FileSystemAccessRule(
    $user, 'FullControl', 'None', 'None', 'Allow')
  $acl.SetAccessRule($allow)
  Set-Acl -LiteralPath $Path -AclObject $acl
}

trap {
  [Console]::Error.WriteLine("Install failed: $Step")
  break
}

$Hub = ''
$Token = ''
$TokenFile = ''
$BinSrc = ''
for ($i = 0; $i -lt $args.Count; ) {
  switch ($args[$i]) {
    '--hub' {
      if ($i + 1 -ge $args.Count) { Fail "--hub requires a URL" }
      $Hub = [string]$args[$i + 1]
      $i += 2
    }
    '--token' {
      if ($i + 1 -ge $args.Count) { Fail "--token requires a token" }
      $Token = [string]$args[$i + 1]
      $i += 2
    }
    '--token-file' {
      if ($i + 1 -ge $args.Count) { Fail "--token-file requires a file" }
      $TokenFile = [string]$args[$i + 1]
      $i += 2
    }
    '--binary' {
      if ($i + 1 -ge $args.Count) { Fail "--binary requires a file" }
      $BinSrc = [string]$args[$i + 1]
      $i += 2
    }
    '--help' {
      Write-Output "Usage: $($MyInvocation.MyCommand.Name) --hub URL [--token TOKEN | --token-file FILE] [--binary FILE]"
      Write-Output "  --hub http://<tailscale-ipv4>:<port>  (same address as the operator UI)"
      Write-Output "  --hub https://<hostname>[:port]       (experimental Cloudflare Tunnel; agent check-in only)"
      exit 0
    }
    default { Fail "Unknown option: $($args[$i])" }
  }
}

function Test-HubOctet([string]$Octet) {
  if ($Octet -notmatch '^[0-9]+$') { return $false }
  if ($Octet.Length -gt 1 -and $Octet.StartsWith('0')) { return $false }
  $n = [int]$Octet
  return ($n -ge 0 -and $n -le 255)
}

function Test-HubPort([string]$Port) {
  if ($Port -notmatch '^[0-9]+$') { return $false }
  if ($Port.Length -gt 1 -and $Port.StartsWith('0')) { return $false }
  $n = [int]$Port
  return ($n -ge 1 -and $n -le 65535)
}

function ConvertTo-AgentHubUrl([string]$Raw) {
  if ($Raw -match '[@?#\s]') { Fail "--hub must not include userinfo, a query, or a fragment" }
  if ($Raw.EndsWith('/')) { $Raw = $Raw.Substring(0, $Raw.Length - 1) }
  if ($Raw.StartsWith('http://')) {
    $rest = $Raw.Substring('http://'.Length)
    if ($rest.Contains('/')) { Fail "--hub must not include a path" }
    $colon = $rest.LastIndexOf(':')
    if ($colon -lt 1) { Fail "--hub http URL must be a Tailscale IPv4 and an explicit port" }
    $ip = $rest.Substring(0, $colon)
    $port = $rest.Substring($colon + 1)
    $parts = $ip.Split('.')
    if ($parts.Length -ne 4) { Fail "--hub http URL must be a Tailscale IPv4 and an explicit port" }
    foreach ($part in $parts) {
      if (-not (Test-HubOctet $part)) { Fail "--hub http URL must be a Tailscale IPv4 and an explicit port" }
    }
    if (-not (Test-HubPort $port)) { Fail "--hub http URL must be a Tailscale IPv4 and an explicit port" }
    $o1 = [int]$parts[0]; $o2 = [int]$parts[1]; $o3 = [int]$parts[2]
    if ($o1 -ne 100 -or $o2 -lt 64 -or $o2 -gt 127) { Fail "--hub http URL must be a Tailscale node IPv4" }
    if ($ip -eq '100.100.100.100' -or ($o2 -eq 115 -and ($o3 -eq 92 -or $o3 -eq 93))) {
      Fail "--hub http URL must be a Tailscale node IPv4"
    }
    return "http://${ip}:${port}"
  }
  if ($Raw.StartsWith('https://')) {
    $rest = $Raw.Substring('https://'.Length)
    if ($rest.Contains('/')) { Fail "--hub must not include a path" }
    $HubHost = $rest
    $port = ''
    $colon = $rest.LastIndexOf(':')
    if ($colon -ge 0) {
      $HubHost = $rest.Substring(0, $colon)
      $port = $rest.Substring($colon + 1)
    }
    $HubHost = $HubHost.ToLowerInvariant()
    if ($HubHost -notmatch '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$' -or $HubHost -notmatch '[a-z]' -or $HubHost -match '(^|\.)localhost($|\.)') {
      Fail "--hub https URL must be a DNS hostname"
    }
    if ($port -ne '') {
      if (-not (Test-HubPort $port)) { Fail "--hub https URL must be a DNS hostname and an explicit port" }
      return "https://${host}:${port}"
    }
    return "https://${host}"
  }
  Fail "--hub must be http://<tailscale-ipv4>:<port> or https://<hostname>"
}

$Step = 'preflight'
if ([string]::IsNullOrWhiteSpace($Hub)) { Fail "--hub URL is required" }
$Hub = ConvertTo-AgentHubUrl $Hub
# Ops test hook: validate --hub and stop. Not an operator feature.
if ($env:CLAWCTL_INSTALL_AGENT_CHECK_HUB -eq '1') {
  Write-Output $Hub
  exit 0
}
if ($env:OS -ne 'Windows_NT') {
  Fail "This installer requires Windows. On Linux run ./install-agent.sh. On macOS run ./install-agent-macos.sh"
}
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
if ($null -eq $identity -or $identity.User -eq $null) {
  Fail "Current user security identifier is unavailable"
}
if ($identity.User.IsWellKnown([System.Security.Principal.WellKnownSidType]::LocalSystemSid)) {
  Fail "Run this installer as the user account that will run the agent"
}
if (-not [string]::IsNullOrEmpty($Token) -and -not [string]::IsNullOrEmpty($TokenFile)) {
  Fail "Use only one of --token and --token-file"
}

$AgentArch = ''
try {
  switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { $AgentArch = 'amd64' }
    'Arm64' { $AgentArch = 'arm64' }
    default { Fail "Unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
  }
} catch {
  Fail "Native Windows architecture is unavailable"
}

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$TaskTemplate = Join-Path $ScriptDir 'clawctl-agent.task.xml'
if (-not (Test-Path -LiteralPath $TaskTemplate -PathType Leaf)) {
  Fail "Scheduled task template must be a regular file: $TaskTemplate"
}

if ([string]::IsNullOrEmpty($BinSrc)) {
  foreach ($candidate in @(
      (Join-Path $ScriptDir 'clawctl-agent.exe'),
      (Join-Path $ScriptDir "..\build\clawctl-agent-windows-$AgentArch"),
      (Join-Path $ScriptDir '..\build\clawctl-agent.exe')
    )) {
    if (Test-Path -LiteralPath $candidate -PathType Leaf) {
      $BinSrc = $candidate
      break
    }
  }
}
if ([string]::IsNullOrEmpty($BinSrc) -or -not (Test-Path -LiteralPath $BinSrc -PathType Leaf)) {
  Fail "Agent binary not found; use --binary FILE"
}

$BinDir = Join-Path $env:LOCALAPPDATA 'clawctl\bin'
$BinFile = Join-Path $BinDir 'clawctl-agent.exe'
$ConfigDir = Join-Path $env:APPDATA 'clawctl'
$ConfigFile = Join-Path $ConfigDir 'agent.json'
$TaskName = 'clawctl-agent'
$TaskPath = '\clawctl\'

function New-ManagedDirectory([string]$Path) {
  if (Test-Path -LiteralPath $Path) {
    $item = Get-Item -LiteralPath $Path
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
      Fail "Directory must not be a reparse point: $Path"
    }
    if (-not $item.PSIsContainer) {
      Fail "Path exists and is not a directory: $Path"
    }
    return
  }
  New-Item -ItemType Directory -Path $Path | Out-Null
}

$Step = 'agent install'
New-ManagedDirectory $BinDir
New-ManagedDirectory $ConfigDir
$AgentVersion = (& $BinSrc version)
if ([string]::IsNullOrWhiteSpace($AgentVersion)) { Fail "Agent version is empty" }
Copy-Item -LiteralPath $BinSrc -Destination ($BinFile + '.new') -Force
Move-Item -LiteralPath ($BinFile + '.new') -Destination $BinFile -Force
Write-Output "Agent installed: $AgentVersion"

$Step = 'agent enrollment'
$CreatedTokenFile = $null
try {
  if (-not (Test-Path -LiteralPath $ConfigFile)) {
    if (-not [string]::IsNullOrEmpty($TokenFile)) {
      if (-not (Test-Path -LiteralPath $TokenFile -PathType Leaf)) {
        Fail "Token file must be a regular file: $TokenFile"
      }
    } else {
      if ([string]::IsNullOrEmpty($Token)) {
        $secure = Read-Host -Prompt 'Enrollment token' -AsSecureString
        $Token = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
          [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure))
      }
      if ([string]::IsNullOrWhiteSpace($Token)) { Fail "Enrollment token is required" }
      $CreatedTokenFile = Join-Path ([IO.Path]::GetTempPath()) ("clawctl-token-" + [guid]::NewGuid().ToString('N'))
      Set-Content -LiteralPath $CreatedTokenFile -Value $Token -Encoding ascii -NoNewline
      $Token = ''
      Set-PrivateFile $CreatedTokenFile
      $TokenFile = $CreatedTokenFile
    }
    & $BinFile enroll --hub $Hub --token-file $TokenFile
    if ($LASTEXITCODE -ne 0) { Fail "Enrollment failed" }
    if (-not (Test-Path -LiteralPath $ConfigFile -PathType Leaf)) {
      Fail "Enrollment did not create a regular config file: $ConfigFile"
    }
  } elseif ((Get-Item -LiteralPath $ConfigFile).Attributes -band [IO.FileAttributes]::ReparsePoint) {
    Fail "Agent config must be a regular file: $ConfigFile"
  }
} finally {
  if ($CreatedTokenFile -and (Test-Path -LiteralPath $CreatedTokenFile)) {
    Remove-Item -LiteralPath $CreatedTokenFile -Force
  }
}

$Step = 'scheduled task'
$TaskXml = [IO.File]::ReadAllText($TaskTemplate)
$TaskXml = $TaskXml.Replace('@@CLAWCTL_AGENT_BIN@@', (XmlEscape $BinFile))
if ($TaskXml.Contains('@@')) { Fail "Scheduled task XML contains an unresolved placeholder" }
Unregister-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Confirm:$false -ErrorAction SilentlyContinue
Register-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Xml $TaskXml -Force | Out-Null
$ServiceStartedAt = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
Start-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath

$Step = 'Hub readiness'
& $BinFile verify --hub $Hub --since $ServiceStartedAt --timeout 2m --require-platform-evidence
if ($LASTEXITCODE -ne 0) { Fail "Hub readiness verification failed" }

Write-Output "Managed: agent=$AgentVersion service=$TaskPath$TaskName config=$ConfigFile"
