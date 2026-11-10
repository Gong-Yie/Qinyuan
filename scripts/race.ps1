<#
.SYNOPSIS
  用 64 位 Go + 可用的 mingw-w64 GCC 跑竞态检测。

.DESCRIPTION
  为什么需要单独一个脚本：
    - `go test -race` 需要 cgo，而本机默认 PATH 里的 gcc 是 SageMath 自带的 10.2，
      它与 Go race runtime 的 cgo 不兼容（-Werror=format 编译失败）；
    - 真正可用的是 E:\toolchains\mingw64\bin\gcc.exe（MinGW-W64 16.2.0）。

  这个脚本只负责：找到一个能用的 gcc → 设 CC/CGO_ENABLED → 交给 scripts/go.ps1。

  用法：
    pwsh -File scripts/race.ps1
    pwsh -File scripts/race.ps1 ./internal/store/...
    pwsh -File scripts/race.ps1 ./... -count=3
#>
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$goScript = Join-Path $root 'scripts\go.ps1'

$gccCandidates = @(
    'E:\toolchains\mingw64\bin\gcc.exe',
    'C:\msys64\mingw64\bin\gcc.exe',
    'C:\TDM-GCC-64\bin\gcc.exe',
    'C:\mingw64\bin\gcc.exe'
)

$gcc = $null
foreach ($c in $gccCandidates) {
    if (Test-Path $c) { $gcc = $c; break }
}

if (-not $gcc) {
    # 退化：PATH 里若有 gcc 也试一次，但会明确提示可能是 SageMath 那份。
    $cmd = Get-Command gcc -ErrorAction SilentlyContinue
    if ($cmd) {
        Write-Warning "未找到已知可用的 mingw-w64，退回 PATH 里的 gcc：$($cmd.Source)"
        $gcc = $cmd.Source
    } else {
        throw "没有找到 gcc。请安装 mingw-w64，或把 gcc 放到：$($gccCandidates -join ' / ')"
    }
}

$env:CC = $gcc
$env:CGO_ENABLED = '1'

# 注意：不要写成 `$targets = if (...) { $args } else { @('./...') }` ——
# PowerShell 的 if 表达式会把**单元素数组解包成字符串**，随后 `@targets` 会把字符串
# 按字符展开（"./..." 变成 "."、"/"、"."、"..." 四个包，go test 直接报 no Go files）。
$targets = @('./...')
if ($args.Count -gt 0) { $targets = $args }
Write-Host "[race] CC=$gcc"
& pwsh -NoProfile -File $goScript test -race @targets
exit $LASTEXITCODE
