<#
.SYNOPSIS
  钦原开发用 PostgreSQL 便携实例（数据目录在项目 .devdb/，不装服务、不需要管理员）。

.DESCRIPTION
  首次使用：
  pwsh -File scripts/devdb.ps1 init      # 解包 + initdb + 建库（幂等）
    pwsh -File scripts/devdb.ps1 start
    pwsh -File scripts/devdb.ps1 status
    pwsh -File scripts/devdb.ps1 stop
    pwsh -File scripts/devdb.ps1 reset     # 停库 → 删数据目录 → 重新 init

  默认：127.0.0.1:55432，用户 postgres，trust 认证，库 qinyuan / qinyuan_test。
  运行时与数据目录放在 %LOCALAPPDATA%\qinyuan（ASCII 路径）：
  PostgreSQL 的 initdb 在 Windows 上无法处理含中文的数据目录（bootstrap 脚本按字节
  读取路径，GBK 字节会被判成非法 UTF-8），所以不能放在项目的中文路径下。
  这些默认值只用于本地开发，生产请用独立实例并改 QINYUAN_DSN。
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('init', 'start', 'stop', 'status', 'reset', 'psql', 'serve')]
    [string]$Action = 'status',
    [string]$PsqlArgs = ''
)

$ErrorActionPreference = 'Stop'

$Root    = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$DevDir  = Join-Path $Root '.devdb'
$PgHome  = Join-Path $env:LOCALAPPDATA 'qinyuan'
$PgDir   = Join-Path $PgHome 'pg\pgsql'
$BinDir  = Join-Path $PgDir 'bin'
$DataDir = Join-Path $PgHome 'data'
$ZipPath = Join-Path $DevDir 'cache\pg17.zip'
$LogFile = Join-Path $PgHome 'pg.log'
$Port    = 55432
$User    = 'postgres'

function Get-PgCtl { Join-Path $BinDir 'pg_ctl.exe' }

function Test-Ready {
    if (-not (Test-Path (Join-Path $BinDir 'pg_ctl.exe'))) { return $false }
    if (-not (Test-Path (Join-Path $DataDir 'PG_VERSION'))) { return $false }
    return $true
}

function Invoke-Initdb {
    if (-not (Test-Path (Join-Path $BinDir 'initdb.exe'))) {
        throw "未找到 PostgreSQL 二进制：$BinDir。请先下载并解包，或手动放到 .devdb/pg。"
    }
    if (Test-Path (Join-Path $DataDir 'PG_VERSION')) {
        Write-Host '[devdb] data 目录已存在，跳过 initdb'
        return
    }
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
    Write-Host "[devdb] initdb -> $DataDir"
    & (Join-Path $BinDir 'initdb.exe') -D $DataDir -U $User -A trust -E UTF8 --locale=C | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "initdb 失败（exit $LASTEXITCODE）" }
}

function Invoke-Start {
    $ready = Join-Path $BinDir 'pg_isready.exe'
    & $ready -h 127.0.0.1 -p $Port *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Host '[devdb] 已在运行'
        return
    }

    # 清理上次被强杀留下的 postmaster.pid（否则 postgres 会拒绝启动）
    $pidFile = Join-Path $DataDir 'postmaster.pid'
    if (Test-Path $pidFile) {
        $oldPid = (Get-Content $pidFile | Select-Object -First 1)
        if (-not (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) {
            Write-Host "[devdb] 清理陈旧的 postmaster.pid（pid $oldPid 已不存在）"
            Remove-Item -Force $pidFile
        }
    }

    # 关键：必须脱离当前进程启动，并且把 stdout/stderr 重定向到文件 ——
    # 否则 postgres 会继承调用方的管道，任何 `script | Out-String` 都会等到超时。
    Write-Host "[devdb] 启动 postgres (port $Port)"
    $outLog = Join-Path $PgHome 'pg-stdout.log'
    $errLog = Join-Path $PgHome 'pg-stderr.log'
    $nulIn  = Join-Path $PgHome 'pg-stdin.nul'
    if (-not (Test-Path $nulIn)) { New-Item -ItemType File -Force -Path $nulIn | Out-Null }
    $p = Start-Process -FilePath (Join-Path $BinDir 'postgres.exe') `
        -ArgumentList @('-D', $DataDir, '-p', "$Port", '-c', 'listen_addresses=127.0.0.1') `
        -WindowStyle Hidden -PassThru `
        -RedirectStandardInput $nulIn -RedirectStandardOutput $outLog -RedirectStandardError $errLog

    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 500
        & $ready -h 127.0.0.1 -p $Port *> $null
        if ($LASTEXITCODE -eq 0) {
            Write-Host "[devdb] ready（pid $($p.Id)）"
            return
        }
        if ($p.HasExited) { throw "postgres 启动即退出（exit $($p.ExitCode)），见 $errLog" }
    }
    throw "启动超时，见 $errLog"
}

function Invoke-Stop {
    $ctl = Get-PgCtl
    if (-not (Test-Path $DataDir)) { Write-Host '[devdb] 无数据目录，无需停止'; return }
    & $ctl -D $DataDir status *> $null
    if ($LASTEXITCODE -ne 0) { Write-Host '[devdb] 未在运行'; return }
    & $ctl -D $DataDir -m fast -w stop | Out-Host
}

function Invoke-Createdbs {
    $psql = Join-Path $BinDir 'psql.exe'
    $createdb = Join-Path $BinDir 'createdb.exe'
    foreach ($db in @('qinyuan', 'qinyuan_test')) {
        $exists = & $psql -h 127.0.0.1 -p $Port -U $User -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$db'"
        if ("$exists".Trim() -ne '1') {
            Write-Host "[devdb] createdb $db"
            & $createdb -h 127.0.0.1 -p $Port -U $User $db
        }
    }
}

function Invoke-Status {
    if (-not (Test-Ready)) {
        Write-Host '[devdb] 未初始化（先跑 init）'
        return
    }
    & (Get-PgCtl) -D $DataDir status | Out-Host
    if ($LASTEXITCODE -eq 0) {
        & (Join-Path $BinDir 'psql.exe') -h 127.0.0.1 -p $Port -U $User -d postgres -tAc 'SELECT version()' | Out-Host
    }
}

switch ($Action) {
    'init' {
        if (-not (Test-Path (Join-Path $BinDir 'initdb.exe'))) {
            if (-not (Test-Path $ZipPath)) { throw "缺少 $ZipPath，请先下载 PostgreSQL binaries zip" }
            New-Item -ItemType Directory -Force -Path (Join-Path $PgHome 'pg') | Out-Null
            Write-Host '[devdb] 解包（tar.exe）…'
            & tar.exe -xf $ZipPath -C (Join-Path $PgHome 'pg')
            if ($LASTEXITCODE -ne 0) { throw '解包失败' }
        }
        Invoke-Initdb
        Invoke-Start
        Invoke-Createdbs
        Invoke-Status
    }
    'start' { Invoke-Start; Invoke-Createdbs }
    'stop' { Invoke-Stop }
    'status' { Invoke-Status }
    'reset' {
        Invoke-Stop
        if (Test-Path $DataDir) { Remove-Item -Recurse -Force $DataDir }
        Invoke-Initdb
        Invoke-Start
        Invoke-Createdbs
    }
    'psql' { & (Join-Path $BinDir 'psql.exe') -h 127.0.0.1 -p $Port -U $User -d qinyuan $PsqlArgs.Split(' ') }
    # serve：前台运行（Ctrl+C 停止）。在被进程组/作业对象管理的环境里（CI、agent 工具），
    # 用 serve 比 start 可靠 —— start 派生的 postgres 会随调用方退出而被带走。
    'serve' {
        if (-not (Test-Ready)) { throw '未初始化（先跑 init）' }
        & (Join-Path $BinDir 'postgres.exe') -D $DataDir -p $Port -c listen_addresses=127.0.0.1
    }
}
