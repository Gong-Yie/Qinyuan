# 钦原执行沙箱初始化（M17，docs/24）
#
# 作用：在 WSL2 的 Docker 里准备沙箱网络与镜像。幂等，可重复执行。
# 用法：
#   pwsh -File scripts/sandbox/setup.ps1              # 默认 Ubuntu-24.04
#   pwsh -File scripts/sandbox/setup.ps1 Ubuntu-24.04
#
# 前置：WSL2 已安装、docker 在 WSL 里可用（docker ps 能跑）。

param(
    [string]$Distro = "Ubuntu-24.04",
    [string]$Image = "qinyuan/sandbox:latest",
    [string]$Network = "qy-sandbox",
    [string]$Subnet = "10.201.0.0/24"
)

$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path

function Wsl([string]$distro, [string[]]$extra, [string]$cmd) {
    & wsl.exe -d $distro @extra -- bash -lc $cmd
    if ($LASTEXITCODE -ne 0) { throw "WSL 命令失败（exit $LASTEXITCODE）：$cmd" }
}

Write-Host "[1/3] 检查 WSL + docker ..."
Wsl $Distro @() "docker version --format '{{.Server.Version}}' | sed 's/^/docker /'"

Write-Host "[2/3] 确保网络 $Network ($Subnet) ..."
Wsl $Distro @("-u", "root") "docker network inspect $Network >/dev/null 2>&1 || docker network create --subnet $Subnet $Network"

Write-Host "[3/3] 构建镜像 $Image ..."
Wsl $Distro @() "rm -rf /tmp/qy-sandbox-build && mkdir -p /tmp/qy-sandbox-build"
Get-Content (Join-Path $here "Dockerfile") -Raw | & wsl.exe -d $Distro -- bash -lc "cat > /tmp/qy-sandbox-build/Dockerfile"
if ($LASTEXITCODE -ne 0) { throw "写 Dockerfile 失败" }
Wsl $Distro @() "cd /tmp/qy-sandbox-build && docker build -t $Image . 2>&1 | tail -5"

Write-Host ""
Write-Host "完成。在 .env.local 里设置 QINYUAN_SANDBOX=on 后启动 serve。"
Write-Host "回连白名单：POST /api/runs/{id}/callbacks  {host, proto, ports, ttl_seconds}"
