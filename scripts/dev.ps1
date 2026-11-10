# 本地开发用：直接跑 Go 进程，**不构建镜像**。
#
# 改后端：保存后重跑本脚本（go run 增量编译，2~5 秒）。
# 改前端：主界面在 web/（Vue），用 scripts/web-build.sh --dev 起 vite（HMR）；
#         或者 scripts/web-build.sh 构建进二进制后重跑本脚本。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\dev.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\dev.ps1 -FreshGate
#   powershell -ExecutionPolicy Bypass -File scripts\dev.ps1 -Port 7060
#
# 管理员账号密码默认是 admin / devpass（首次启动时写进 .dev\data 的数据库）。
# -FreshGate 表示不给初始密码：打开的页面就是「设置管理员账号」，
# 用来验证首次配置流程。

param(
    [int]$Port = 6060,
    [int]$ProxyPort = 6086,
    [string]$User = 'admin',
    [string]$Password = 'devpass',
    [switch]$FreshGate
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    $guess = 'C:\Users\' + $env:USERNAME + '\go-sdk\go\bin\go.exe'
    if (Test-Path $guess) {
        $env:PATH = (Split-Path $guess -Parent) + ';' + $env:PATH
        $go = Get-Command go -ErrorAction SilentlyContinue
    }
}
if (-not $go) {
    throw 'go not found. Install Go 1.25+ or run setup\1-install.ps1.'
}

$dev = Join-Path $root '.dev'
New-Item -ItemType Directory -Force -Path (Join-Path $dev 'data'), (Join-Path $dev 'media') | Out-Null

# 开发数据与容器数据分开，避免把测试账号写进 ./data
$env:DATA_DIR = Join-Path $dev 'data'
$env:MEDIA_ROOT = Join-Path $dev 'media'
$env:ADDR = ":$Port"
$env:PROXY_ADDR = ":$ProxyPort"
$env:ADMIN_USER = $User
if ($FreshGate) {
    $env:ADMIN_PASSWORD = ''
} else {
    $env:ADMIN_PASSWORD = $Password
}

# 前端从磁盘读，改完刷新浏览器就生效

Write-Host ''
Write-Host '  管理后台  http://127.0.0.1:'$Port -ForegroundColor Green
Write-Host '  播放代理  http://127.0.0.1:'$ProxyPort -ForegroundColor Green
Write-Host '  数据目录  '$env:DATA_DIR -ForegroundColor DarkGray
Write-Host '  媒体目录  '$env:MEDIA_ROOT -ForegroundColor DarkGray
if ($FreshGate) {
    Write-Host '  首次访问请在页面上设置账号密码' -ForegroundColor DarkGray
} else {
    Write-Host ('  登录账号  ' + $env:ADMIN_USER + ' / ' + $env:ADMIN_PASSWORD) -ForegroundColor DarkGray
}
Write-Host '  Ctrl+C 退出；改前端不用重启，改后端重跑本脚本' -ForegroundColor Yellow
Write-Host ''

& $go.Source run ./cmd/server
