<#
.SYNOPSIS
  M4 现场演示：用真实 serve + 真实 OpenAI 适配器（对假网关）跑一遍
  收尾轮 / 并发队列 / 删除补位 / 流式 TTFT。

.DESCRIPTION
  为什么要一个假网关：钦原的 M4 大部分能力发生在"真实模型调用"路径上
  （收尾轮、流式 committed、TTFT）。用一个只实现 chat/completions 的本地网关，
  就能让 serve 走**真实适配器**，而不需要外部 API key。

  它会：
    1. 建独立 schema（m4demo），不碰开发库；
    2. 起假网关 :18080、serve :18787（并发上限=1、MaxTurns=1）；
    3. 建 run A（含一条手动意图）→ 跑出"主循环 + 收尾轮"两条事实；
    4. 建 run B 并启动 → 因上限=1 排队；
    5. 删除 A → B 自动补位；
    6. 打印关键 activity（首字延迟 / 进入收尾轮 / 已出队启动）；
    7. 清理 schema 与进程。

  用法：pwsh -File scripts/demo/demo-m4.ps1
#>
param(
    [int]$GatewayPort = 18080,
    [int]$ServePort = 18787,
    [string]$Schema = "m4demo"
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path))
Set-Location $root

$pgBin = Join-Path $env:LOCALAPPDATA 'qinyuan\pg\pgsql\bin'
$psql = Join-Path $pgBin 'psql.exe'
$baseDSN = "postgres://postgres@127.0.0.1:55432/qinyuan?sslmode=disable"
$dsn = "$baseDSN&search_path=$Schema"
$api = "http://127.0.0.1:$ServePort/api"

function Say([string]$text) { Write-Host ""; Write-Host "── $text " -ForegroundColor Cyan }
function Note([string]$text) { Write-Host "   $text" -ForegroundColor DarkGray }
function Check([string]$text) { Write-Host "   ✓ $text" -ForegroundColor Green }

function Stop-OnPort([int]$port) {
    $conns = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
    foreach ($c in $conns) { try { Stop-Process -Id $c.OwningProcess -Force -ErrorAction SilentlyContinue } catch {} }
}

function Wait-Health([string]$url, [int]$tries = 60) {
    for ($i = 0; $i -lt $tries; $i++) {
        try { Invoke-RestMethod -Uri $url -TimeoutSec 2 | Out-Null; return $true } catch { Start-Sleep -Milliseconds 300 }
    }
    return $false
}

function Get-Run([int64]$id) { Invoke-RestMethod -Uri "$api/runs/$id" }
function Post-Json([string]$path, $obj) {
    Invoke-RestMethod -Method Post -Uri "$api$path" -ContentType 'application/json' `
        -Body ($obj | ConvertTo-Json -Depth 8)
}

# 打印 activity：只挑对本演示有意义的行，避免刷屏。
function Show-Activity([int64]$id, [string]$title) {
    $acts = (Invoke-RestMethod -Uri "$api/runs/$id/activity?limit=200").items
    Say $title
    foreach ($a in $acts) {
        $summary = "$($a.summary)"
        if ($summary.Length -gt 96) { $summary = $summary.Substring(0, 96) + '…' }
        $line = "   {0,-10} {1,-12} {2}" -f $a.kind, $(if ($a.tool) { $a.tool } else { '' }), $summary
        if ($a.kind -eq 'system' -and ($summary -match '首字延迟|收尾轮|出队|排队')) {
            Write-Host $line -ForegroundColor Yellow
        } elseif ($a.kind -in @('tool_use', 'tool_result', 'result', 'round')) {
            Write-Host $line
        }
    }
}

$gw = $null; $serve = $null
try {
    Say "① 建独立 schema $Schema（不碰开发库）"
    & $psql -h 127.0.0.1 -p 55432 -U postgres -d qinyuan -v ON_ERROR_STOP=1 `
        -c "CREATE SCHEMA IF NOT EXISTS $Schema" | Out-Null
    Check "schema 就绪"

    Say "② 迁移（幂等）"
    $env:QINYUAN_DSN = $dsn
    & pwsh -NoProfile -File (Join-Path $root 'scripts\go.ps1') run ./cmd/qinyuan migrate | Out-Null
    Check "迁移完成"

    Say "③ 起假网关 :$GatewayPort 与 serve :$ServePort"
    Stop-OnPort $GatewayPort; Stop-OnPort $ServePort
    $gw = Start-Process pwsh -PassThru -WindowStyle Hidden -ArgumentList @(
        '-NoProfile', '-File', (Join-Path $root 'scripts\go.ps1'),
        'run', './scripts/demo/fakegw.go', '-addr', "127.0.0.1:$GatewayPort"
    )
    if (-not (Wait-Health "http://127.0.0.1:$GatewayPort/health")) { throw "假网关没起来" }

    # serve 的环境：真实 OpenAI 适配器指向本地网关；并发上限 1；每次执行只给 1 回合（必触发收尾轮）
    $env:QINYUAN_ADDR = "127.0.0.1:$ServePort"
    $env:QINYUAN_MAX_CONCURRENT_RUNS = '1'
    $env:QINYUAN_MAX_TURNS = '1'
    $env:QINYUAN_LLM_PROVIDER = 'openai'
    $env:QINYUAN_LLM_BASE_URL = "http://127.0.0.1:$GatewayPort/v1"
    $env:QINYUAN_LLM_MODEL = 'demo'
    $env:QINYUAN_LLM_API_KEY = 'sk-demo'
    $serve = Start-Process pwsh -PassThru -WindowStyle Hidden -ArgumentList @(
        '-NoProfile', '-File', (Join-Path $root 'scripts\go.ps1'),
        'run', './cmd/qinyuan', 'serve'
    )
    if (-not (Wait-Health "http://127.0.0.1:$ServePort/health" 100)) { throw "serve 没起来" }
    Check "网关 + serve 就绪（provider=openai，指向本地假网关）"

    Say "④ 建 run A 并投一条意图，然后启动"
    $a = Post-Json '/runs' @{
        goal        = '确认演示站点的登录入口存在且可访问'
        target      = 'http://demo.local/'
        description = 'M4 现场演示：MaxTurns=1，主循环只够写一条事实，剩下交给收尾轮'
        worker_count = 1
    }
    $aID = $a.id
    Check "run A = $aID"
    Post-Json "/runs/$aID/intents" @{ summary = '请求首页并写回指纹与登录表单'; priority = 6 } | Out-Null
    Post-Json "/runs/$aID/control" @{ action = 'start' } | Out-Null

    Say "⑤ 等 A 跑完（主循环 → 撞 MaxTurns → 收尾轮）"
    Start-Sleep -Seconds 4
    $aNow = Get-Run $aID
    $ov = Invoke-RestMethod -Uri "$api/runs/$aID/overview"
    $openGoals = @($ov.goals | Where-Object { $_.state -eq 'open' }).Count
    $abandoned = @($ov.goals | Where-Object { $_.state -eq 'abandoned' }).Count
    Note ("open 目标 {0} 条（另 {1} 条 seed 已退役）/ 事实 {2} 条（含 origin）/ 意图 done {3} 条" `
        -f $openGoals, $abandoned, $ov.facts_total, $ov.done_total)
    Check "goal 拆解 + worker 执行完成（意图撞 MaxTurns → exhausted，不是 done）"
    Show-Activity $aID "A 的关键 activity"

    Say "⑥ 建 run B 并启动 → 并发上限=1，应该排队"
    $b = Post-Json '/runs' @{
        goal = '确认演示站点 2 的登录入口'; target = 'http://demo2.local/'; worker_count = 1
    }
    $bID = $b.id
    Post-Json "/runs/$bID/intents" @{ summary = '请求首页并写回指纹'; priority = 6 } | Out-Null
    Post-Json "/runs/$bID/control" @{ action = 'start' } | Out-Null
    Start-Sleep -Milliseconds 500
    $bNow = Get-Run $bID
    Note ("B: queued={0} task_status={1} derive_status={2}" -f $bNow.queued, $bNow.task_status, $bNow.status)
    if (-not $bNow.queued) { throw "B 应当排队，实际没有" }
    Check "B 已进入持久化队列（queued_at 保位）"

    Say "⑦ 删除 A（真实删除流水线）→ B 自动补位"
    $del = Invoke-RestMethod -Method Delete -Uri "$api/runs/$aID"
    Note ("DELETE /api/runs/$aID → deleted={0}" -f $del.deleted)
    $deadline = (Get-Date).AddSeconds(15)
    $promoted = $false
    while ((Get-Date) -lt $deadline) {
        $bNow = Get-Run $bID
        if (-not $bNow.queued) { $promoted = $true; break }
        Start-Sleep -Milliseconds 300
    }
    if (-not $promoted) { throw "B 没有被补位" }
    Start-Sleep -Seconds 3
    $bNow = Get-Run $bID
    Check ("B 已出队并运行：queued={0} task_status={1}" -f $bNow.queued, $bNow.task_status)
    Show-Activity $bID "B 的关键 activity（含「已出队启动」）"
    try {
        Get-Run $aID | Out-Null
        throw "A 应该已经不存在"
    } catch {
        if ($_.Exception.Response.StatusCode.value__ -eq 404) { Check "A 已从库中删除（GET 返回 404）" }
        else { throw }
    }
}
finally {
    Say "⑧ 清理"
    Stop-OnPort $ServePort
    Stop-OnPort $GatewayPort
    try {
        & $psql -h 127.0.0.1 -p 55432 -U postgres -d qinyuan -v ON_ERROR_STOP=1 `
            -c "DROP SCHEMA IF EXISTS $Schema CASCADE" | Out-Null
        Check "schema $Schema 已删除；进程已停"
    } catch {
        Note "清理 schema 失败（可手动执行 DROP SCHEMA $Schema CASCADE）：$($_.Exception.Message)"
    }
}
