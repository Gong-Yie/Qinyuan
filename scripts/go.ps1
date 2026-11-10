<#
.SYNOPSIS
  用 64 位 Go 执行命令（优先系统安装，其次项目自带便携版）。

.DESCRIPTION
  背景：这台机器原先装的是 windows/386（32 位），既不能用 -race，内存上限也低。
  现在系统里已换成 C:\Program Files\Go（windows/amd64）；本脚本负责：
    1) 优先使用系统 64 位 Go；
    2) 找不到时退回 %LOCALAPPDATA%\qinyuan\tools\go（便携版，无需管理员）；
    3) 两个都不是 64 位就直接报错 —— 免得又悄悄跑回 386。

  用法：
    pwsh -File scripts/go.ps1 version
    pwsh -File scripts/go.ps1 test ./...
    pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo
#>
$ErrorActionPreference = 'Stop'

function Test-Go64([string]$exe) {
    if (-not (Test-Path $exe)) { return $false }
    $ver = & $exe version 2>$null
    return ($ver -match 'windows/amd64|windows/arm64')
}

$candidates = @(
    'C:\Program Files\Go\bin\go.exe',
    (Join-Path $env:LOCALAPPDATA 'qinyuan\tools\go\bin\go.exe')
)

$go = $null
foreach ($c in $candidates) {
    if (Test-Go64 $c) { $go = $c; break }
}

if (-not $go) {
    $found = @()
    foreach ($c in $candidates) {
        if (Test-Path $c) { $found += (& $c version) }
    }
    throw "没有找到 64 位 Go。已找到：$($found -join '; ')。请安装 64 位 Go 或解包便携版到 $($candidates[1])"
}

& $go @args
exit $LASTEXITCODE
