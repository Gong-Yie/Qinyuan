<#
.SYNOPSIS
  用项目自带的 64 位 Go 执行命令（避免依赖系统 PATH 里的 32 位 Go）。

.DESCRIPTION
  首次使用前先准备工具链（一次性，约 75MB）：
    $d = "$env:LOCALAPPDATA\qinyuan\tools"
    New-Item -ItemType Directory -Force $d | Out-Null
    curl.exe -sL -o "$d\go.zip" https://mirrors.aliyun.com/golang/go1.26.2.windows-amd64.zip
    tar.exe -xf "$d\go.zip" -C $d

  用法：
    pwsh -File scripts/go.ps1 version
    pwsh -File scripts/go.ps1 test ./...
    pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo

  为什么不用系统里的 Go：本机默认装的是 windows/386（32 位），
  既不能用 -race，内存上限也低。64 位 Go 只装在用户目录，不需要管理员。
#>
$ErrorActionPreference = 'Stop'

$go = Join-Path $env:LOCALAPPDATA 'qinyuan\tools\go\bin\go.exe'
if (-not (Test-Path $go)) {
    throw "未找到 64 位 Go：$go。先按脚本头部说明下载并解包。"
}

& $go @args
exit $LASTEXITCODE
