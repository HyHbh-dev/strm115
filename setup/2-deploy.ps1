# Stage 2/2 - Run AFTER rebooting (stage 1). Administrator NOT required.
#
# Starts Docker Desktop, prepares .env, deploys strm115 + Emby, and lets the
# one-shot "bootstrap" container finish the wiring:
#   Emby wizard -> API key -> strm115 settings -> 115 account
#   -> library -> full sync -> Emby library + scan
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File D:\code\strm115\setup\2-deploy.ps1
#   ... -EmbyPassword 'your-emby-admin-password'     # skip the prompt
#   ... -NonInteractive                              # never prompt
#
# Everything here is idempotent: re-running reuses the existing account,
# library and API key instead of creating duplicates.

param(
    [string]$EmbyPassword = '',
    [switch]$NonInteractive
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Say($m)  { Write-Host "`n=== $m ===" -ForegroundColor Cyan }
function Good($m) { Write-Host "  [OK] $m" -ForegroundColor Green }
function Note($m) { Write-Host "  [..] $m" -ForegroundColor Yellow }
function Warn($m) { Write-Host "  [!!] $m" -ForegroundColor Red }

$cmd = Get-Command docker -ErrorAction SilentlyContinue
if ($cmd) { $docker = $cmd.Source }
else {
    $docker = 'C:\Program Files\Docker\Docker\resources\bin\docker.exe'
    if (-not (Test-Path $docker)) {
        throw 'Docker CLI not found. Finish stage 1 (1-install.ps1) and reboot first.'
    }
}

# Read/update one KEY=VALUE line in .env, preserving UTF-8 WITHOUT BOM.
# A BOM in front of the first line makes docker compose read it as garbage.
function Set-EnvValue([string]$key, [string]$value) {
    $path = Join-Path $root '.env'
    $lines = @()
    $found = $false
    if (Test-Path $path) {
        $lines = @(Get-Content $path -Encoding UTF8)
        for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i] -match "^$key\s*=") {
                $lines[$i] = "$key=$value"
                $found = $true
                break
            }
        }
    }
    if (-not $found) { $lines += "$key=$value" }
    [System.IO.File]::WriteAllText($path, (($lines -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))
}

function Get-EnvValue([string]$key) {
    $path = Join-Path $root '.env'
    if (-not (Test-Path $path)) { return '' }
    $line = Get-Content $path -Encoding UTF8 | Where-Object { $_ -match "^$key\s*=" } | Select-Object -First 1
    if (-not $line) { return '' }
    return ($line -split '=', 2)[1].Trim()
}

Say 'Step 1/6  Start Docker Desktop'
$dd = 'C:\Program Files\Docker\Docker\Docker Desktop.exe'
if (-not (Test-Path $dd)) { throw 'Docker Desktop is not installed. Run setup\1-install.ps1 first.' }
if (-not (Get-Process -Name 'Docker Desktop' -ErrorAction SilentlyContinue)) {
    Start-Process -FilePath $dd
    Note 'Launched Docker Desktop; waiting for the engine (first launch can take a few minutes)'
}

Say 'Step 2/6  Wait for the Docker engine'
$deadline = (Get-Date).AddMinutes(8)
$ready = $false
while ((Get-Date) -lt $deadline) {
    & $docker info 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    Start-Sleep -Seconds 5
}
if (-not $ready) {
    throw 'Docker engine did not become ready in 8 minutes. Open Docker Desktop, finish the first-run wizard / accept the license, then re-run this script.'
}
Good 'Docker engine is ready'

Say 'Step 3/6  Prepare .env'
if (Test-Path '.env') {
    Note '.env already exists, keeping its values'
} else {
    # -Encoding UTF8 matters: Windows PowerShell 5.1 defaults to the ANSI code
    # page, which mangles the UTF-8 Chinese comments in these files.
    $txt = Get-Content '.env.example' -Raw -Encoding UTF8
    [System.IO.File]::WriteAllText((Join-Path $root '.env'), $txt, (New-Object System.Text.UTF8Encoding($false)))
    Good '.env created'
}
Note 'Admin account is created in the browser on first visit: http://localhost:6060'

# --- PLAY_DOMAIN: this is what users actually get wrong ---------------------
# It must be a LAN address the *player* can reach. 127.0.0.1 only works on this
# machine; a container IP works for nobody.
$currentDomain = Get-EnvValue 'PLAY_DOMAIN'
if (-not $currentDomain -or $currentDomain -match '192\.168\.1\.10:6086$') {
    $ip = $null
    try {
        $cand = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
                Where-Object { $_.IPAddress -notlike '169.*' -and $_.IPAddress -notlike '127.*' -and $_.PrefixOrigin -ne 'WellKnown' }
        if ($cand) { $ip = ($cand | Select-Object -First 1).IPAddress }
    } catch { }
    if ($ip) {
        $domain = "http://${ip}:6086"
        Set-EnvValue 'PLAY_DOMAIN' $domain
        Good "PLAY_DOMAIN = $domain"
    } else {
        Warn 'No LAN IP detected. Set PLAY_DOMAIN in .env manually (http://<your-ip>:6086)'
    }
} else {
    Good "PLAY_DOMAIN = $currentDomain (from .env)"
}

# --- Emby admin password: only needed once, to create the API key -----------
$embyPw = Get-EnvValue 'EMBY_ADMIN_PASSWORD'
$embyKey = Get-EnvValue 'EMBY_API_KEY'
if (-not $EmbyPassword) { $EmbyPassword = $embyPw }
if (-not $EmbyPassword -and -not $embyKey -and -not $NonInteractive) {
    Write-Host ''
    Write-Host '  The Emby admin password is used ONCE, to create an API key.' -ForegroundColor Yellow
    Write-Host '  Leave it empty to skip Emby wiring (you can add EMBY_API_KEY later).' -ForegroundColor Yellow
    $sec = Read-Host '  Emby admin password' -AsSecureString
    if ($sec.Length -gt 0) {
        $EmbyPassword = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
    }
}
if ($EmbyPassword) {
    Set-EnvValue 'EMBY_ADMIN_PASSWORD' $EmbyPassword
    Good 'EMBY_ADMIN_PASSWORD saved to .env'
} elseif (-not $embyKey) {
    Note 'No Emby credentials - Emby wiring will be skipped (everything else still runs)'
}

$cid = Get-EnvValue 'PAN115_CID'
if (-not $cid) {
    Note 'PAN115_CID is empty: bootstrap will list your 115 folders, pick one and re-run'
}

Say 'Step 4/6  Build and start (strm115 + Emby)'
& $docker compose --profile emby up -d --build
if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
Good 'Containers started'

Say 'Step 5/6  Wait for strm115 to become healthy'
$deadline = (Get-Date).AddMinutes(5)
$state = ''
while ((Get-Date) -lt $deadline) {
    $state = (& $docker inspect -f '{{.State.Health.Status}}' strm115 2>$null)
    if ($state -eq 'healthy') { break }
    Start-Sleep -Seconds 3
}
if ($state -ne 'healthy') {
    Warn 'strm115 is not healthy yet - check: docker compose logs -f strm115'
} else {
    Good 'strm115 is healthy'
}

Say 'Step 6/6  Next: finish in the browser'
Write-Host ''
Write-Host '========================================================' -ForegroundColor Green
Write-Host '  strm115 admin : http://127.0.0.1:6060' -ForegroundColor Green
Write-Host '  Emby          : http://127.0.0.1:8096' -ForegroundColor Green
Write-Host "  STRM domain   : $(Get-EnvValue 'PLAY_DOMAIN')" -ForegroundColor Green
Write-Host ''
Write-Host '  Open the admin URL above. On the first visit you will be asked' -ForegroundColor Yellow
Write-Host '  to create the admin account and password - nothing to edit in .env.' -ForegroundColor Yellow
Write-Host ''
Write-Host '  Then, still in the browser:' -ForegroundColor Yellow
Write-Host '    1) Accounts  -> add your 115 account (QR code or cookie)' -ForegroundColor Yellow
Write-Host '    2) Libraries -> pick a 115 folder, then click Sync' -ForegroundColor Yellow
Write-Host '    3) Settings  -> play domain + Emby address / API key' -ForegroundColor Yellow
Write-Host ''
Write-Host '  Fully automated (optional, needs ADMIN_PASSWORD in .env):' -ForegroundColor DarkGray
Write-Host '    docker compose --profile auto run --rm bootstrap' -ForegroundColor DarkGray
Write-Host '========================================================' -ForegroundColor Green
