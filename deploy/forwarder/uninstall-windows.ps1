<#
.SYNOPSIS
  Remove the Windows services installed by install-windows.ps1.
#>
param(
  [string[]]$Services = @('dsh-forwarder-client', 'dsh-tunnel'),
  [switch]$RemoveFiles,
  [string]$InstallDir = "$env:ProgramData\dsh-forwarder"
)
$ErrorActionPreference = 'Stop'
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$p = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'Run PowerShell as Administrator.'
}
foreach ($name in $Services) {
  & sc.exe query $name *> $null
  if ($LASTEXITCODE -eq 0) {
    & sc.exe stop $name | Out-Null
    Start-Sleep -Seconds 2
    & sc.exe delete $name | Out-Null
    Write-Host "removed service $name"
  } else {
    Write-Host "service $name not present"
  }
}
if ($RemoveFiles -and (Test-Path $InstallDir)) {
  Remove-Item -Recurse -Force $InstallDir
  Write-Host "removed $InstallDir"
}
