<#
.SYNOPSIS
  Install the self-written dsh-forwarder-client (and optionally dsh-tunnel)
  as Windows services. Linux uses the systemd units in this directory instead.

.DESCRIPTION
  The plugin's service abstraction maps:
    Linux  -> systemd user/system units (systemctl)
    Windows-> Windows services (sc.exe)
  Service names default to `dsh-forwarder-client` and `dsh-tunnel`, matching
  the plugin's Windows defaults.

.EXAMPLE
  .\install-windows.ps1 -ForwarderExe .\dsh-forwarder-client.exe -Config .\client.json
  .\install-windows.ps1 -TunnelExe .\dsh-tunnel.exe -TunnelArgs '--relay wss://... --device-id win-node --local 127.0.0.1:3082'
#>
param(
  [string]$ForwarderExe,
  [string]$Config,
  [string]$TunnelExe,
  [string]$TunnelArgs,
  [string]$InstallDir = "$env:ProgramData\dsh-forwarder"
)

$ErrorActionPreference = 'Stop'
function Require-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $p = New-Object Security.Principal.WindowsPrincipal($id)
  if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run PowerShell as Administrator.'
  }
}
Require-Admin

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

function Install-Service([string]$Name, [string]$Exe, [string]$Arguments) {
  $target = Join-Path $InstallDir (Split-Path $Exe -Leaf)
  Copy-Item -Force $Exe $target
  $binPath = "`"$target`" $Arguments"
  # Remove a previous instance so the binPath is refreshed.
  & sc.exe query $Name *> $null
  if ($LASTEXITCODE -eq 0) { & sc.exe delete $Name | Out-Null; Start-Sleep -Seconds 2 }
  & sc.exe create $Name binPath= $binPath start= auto DisplayName= $Name | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "sc create $Name failed" }
  & sc.exe start $Name | Out-Null
  Start-Sleep -Seconds 2
  $state = & sc.exe query $Name
  Write-Host "--- $Name ---"
  $state | Select-String 'STATE'
}

if ($ForwarderExe) {
  if (-not $Config) { throw '-Config is required with -ForwarderExe' }
  $cfgTarget = Join-Path $InstallDir 'client.json'
  Copy-Item -Force $Config $cfgTarget
  Install-Service -Name 'dsh-forwarder-client' -Exe $ForwarderExe -Arguments "--config `"$cfgTarget`""
}

if ($TunnelExe) {
  if (-not $TunnelArgs) { throw '-TunnelArgs is required with -TunnelExe' }
  Install-Service -Name 'dsh-tunnel' -Exe $TunnelExe -Arguments $TunnelArgs
}

Write-Host ''
Write-Host 'Installed. The plugin config can now reference these Windows services:'
Write-Host '  services:'
Write-Host '    - id: tunnel'
Write-Host '      kind: windows-service'
Write-Host '      name: dsh-tunnel'
Write-Host '    - id: forwarder'
Write-Host '      kind: windows-service'
Write-Host '      name: dsh-forwarder-client'
