# Stage 1/2 - Run ONCE as Administrator, then REBOOT Windows.
# Enables WSL2 platform features, installs the WSL package and Docker Desktop.
#
# Usage (from an ELEVATED PowerShell):
#   powershell -ExecutionPolicy Bypass -File D:\code\strm115\setup\1-install.ps1
#
#Requires -RunAsAdministrator

$ErrorActionPreference = 'Stop'

function Say($m)  { Write-Host "`n=== $m ===" -ForegroundColor Cyan }
function Good($m) { Write-Host "  [OK] $m" -ForegroundColor Green }
function Note($m) { Write-Host "  [..] $m" -ForegroundColor Yellow }

if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    throw 'winget not found. Install "App Installer" from the Microsoft Store first.'
}

Say 'Step 1/3  Enable WSL + VirtualMachinePlatform (Windows features)'
$reboot = $false
foreach ($f in @('Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')) {
    $state = (Get-WindowsOptionalFeature -Online -FeatureName $f).State
    if ($state -eq 'Enabled') {
        Good "$f is already enabled"
    } else {
        $r = Enable-WindowsOptionalFeature -Online -FeatureName $f -All -NoRestart
        if ($r.RestartNeeded -eq 'Yes') { $reboot = $true }
        Good "$f enabled"
    }
}

Say 'Step 2/3  Install WSL (store build, ships the WSL2 kernel)'
winget install --id Microsoft.WSL --exact `
    --accept-source-agreements --accept-package-agreements --disable-interactivity
if ($LASTEXITCODE -ne 0) { throw "winget failed to install WSL (exit $LASTEXITCODE)" }
Good 'WSL installed'

# Note: do NOT call wsl.exe here. Before the reboot the WSL2 kernel is not
# loaded yet, and wsl.exe may pop a "enable the feature" dialog and hang the
# script. Docker Desktop configures the WSL2 backend itself on first launch.

Say 'Step 3/3  Install Docker Desktop (WSL2 backend)'
winget install --id Docker.DockerDesktop --exact `
    --accept-source-agreements --accept-package-agreements --disable-interactivity
if ($LASTEXITCODE -ne 0) { throw "winget failed to install Docker Desktop (exit $LASTEXITCODE)" }
Good 'Docker Desktop installed'

Write-Host ''
Write-Host '========================================================' -ForegroundColor Yellow
Write-Host '  REBOOT REQUIRED before Docker can start.' -ForegroundColor Yellow
Write-Host '  After rebooting, run stage 2 (no admin needed):' -ForegroundColor Yellow
Write-Host '    powershell -ExecutionPolicy Bypass -File D:\code\strm115\setup\2-deploy.ps1' -ForegroundColor Yellow
Write-Host '========================================================' -ForegroundColor Yellow
if ($reboot) { Note 'Windows feature changes are pending; the reboot is mandatory.' }
