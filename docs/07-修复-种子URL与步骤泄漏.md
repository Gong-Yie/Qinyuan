# 修复 · 种子 URL 丢失（P0）与计划步骤卡 running（P1）

> 来源：真实模型 live 冒烟（2026-11-10，`deepseek-flash` + 本机回环靶场）。
> 前一轮：`docs/06`（M4c 流式与 committed）。本文记录**实测抓到的两个缺陷**及修复，
> 附同一基准的改前/改后对照。

---

## 1. P0 · 入口 URL 在建任务时被丢掉

### 症状

`--target http://127.0.0.1:8080/` 建的任务，worker 不知道端口：

```
http_request GET http://127.0.0.1/       → connection refused
http_request GET https://127.0.0.1/      → connection refused
http_request GET http://127.0.0.1:3000/  → connection refused
… 依次猜 80/443/3000/8000/5000/8001/8081/8888/9000
```

一次任务 45 个 HTTP 请求里 **22 个（49%）打在错误端口**。这些失败不产生信息，只烧 token 和轮次。

### 根因

`internal/runplan/runplan.go` 把 target 降成"范围"时，IP 字面量分支只登记
`run_scope(kind=ip, value=127.0.0.1)` 与 `ip` 实体，**scheme / port / path 全部丢弃**；
库里没有任何一处记着 `:8080`。worker 的 system prompt 只拿得到"目标实体 JSON"
（`agent/prompt.go`），于是只能猜。

**概念错位**：范围回答"允许打哪"，不回答"从哪开始"。这两个问题在数据模型里被合并成了一个。

### 修法

`runs.seeds TEXT[]`（幂等 `ADD COLUMN IF NOT EXISTS`），只做三件事：

1. `runplan.Build` 的 URL 分支把归一化后的 URL 塞进 `CreateRunInput.Seeds`；
2. `store.CreateRun` 写进 `runs.seeds`，`Run()` / `ListRuns()` 读回；
3. `BuildPlannerSystem` / `BuildWorkerSystem` 各渲染一行"起点 URL（端口/协议以它为准，不要猜）"。

顺手修了一个同源 bug：`http://` 前缀判断是大小写敏感的，`HTTP://example.test:80/` 会被
当成裸主机名处理（既不登记 URL，也解析不出端口）。

### 验证（同一 goal、同一靶场）

| 指标 | 改前 run #18 | 改后 run #20 |
|---|---|---|
| HTTP 请求 | 45 | 25 |
| 其中失败 | 22（49%） | **0（0%）** |
| 重复 URL 数 | 9 | 1 |
| 首个请求 | `http://127.0.0.1/`（错端口） | `http://127.0.0.1:8080/`（对） |
| 结局 | done | done |

> 注意 goal 完全相同（**都不含 URL**）：差别只在 `--target`，证明修复点在"建任务",
> 不需要用户改 prompt 习惯。

---

## 2. P1 · 意图结束后计划步骤永远卡在 running

### 症状

run #19 的 `step#82`（"Server 响应头与框架/版本指纹的原始响应证据"）在任务 `done` 后仍是
`running`：它挂着两条 `realized_by` 意图，一条 `done`、一条 `stopped`。计划图于是显示
"还有一步在跑"，而实际上什么都没在跑。

### 根因

`LinkStepIntent` 只负责 `ready/todo → running`；`running` 的**唯一出口**原本是
"事实命中 `expected_evidence`"或"planner 显式 `mark_step_done`"。意图正常跑完、但事实措辞
匹配不上（P2）时，就没有任何人把它推出来。

### 修法

`store.CASIntent` 在意图落到终态（`done/blocked/exhausted/stopped/deleted`）时，同一个事务里
回收挂它的 `running` 步骤（`store/step_reconcile.go`）：

- 已有 `satisfies` 边 → `done`；
- 还有别的 `open/running` 意图挂着同一步 → 保持 `running`（并行意图还在干）；
- 否则 → 依赖都 done 就 `ready`（可重派），否则 `todo`。

**`paused` 不算终态**：它还会被 resume 续跑，步骤保持 `running` 才不会诱导 planner 另派一条
重复意图。改动的步骤与意图共用一个 `rev`（一次写事件，一个版本号）。

### 验证

`run #20`（修复后）任务终态时 `running` 步骤数 = **0**；`run #19`（修复前）= 1。
其中 `step#102` 正是"意图被 goal_met 取消（stopped）后松手到 ready"的实例。

---

## 3. 测试

| 用例 | 守什么 |
|---|---|
| `runplan.TestBuildKeepsSeedURL` | URL 目标保留端口/路径，且范围仍是最小范围 |
| `runplan.TestBuildNormalizesSeedURL` | 默认端口归一化 + scheme 大小写不敏感 |
| `runplan.TestBuildWithoutURLHasNoSeeds` | 裸域名不凭空造协议 |
| `store.TestCreateRunKeepsSeeds` | seeds 落库并可从 `Run()`/`ListRuns()` 读回 |
| `store.TestCreateRunWithoutSeeds` | 空 seeds 读成空切片（不是 NULL） |
| `store.TestIntentTerminalReleasesLinkedStep` | 意图 done → 步骤回 ready |
| `store.TestIntentTerminalKeepsStepWhileSiblingRuns` | 并行意图未结束时保持 running |
| `store.TestIntentTerminalMarksStepDoneWhenSatisfied` | 已有 satisfies → done |
| `store.TestIntentTerminalRespectsDependencies` | 前置未完成 → todo（不提前 ready） |
| `store.TestIntentPausedKeepsStepRunning` | paused 不触发回收 |
| `agent.TestPromptsRenderSeedURL` / `TestPromptsOmitSeedLineWithoutSeeds` | 两个 prompt 都渲染、无 seeds 时不多一行 |

`go test ./... -count=1` 全绿（80 个用例），`scripts/race.ps1 ./...` 零数据竞争。

---

## 4. 本轮没修（已定位，按优先级）

| # | 问题 | 现象 | 方向 |
|---|---|---|---|
| P2 | `expected_evidence` 规则命中率≈0 | 10 步全靠 `mark_step_done`（`satisfied_by_rule=false`） | 按 `、，/或` 切子句后"任一命中"；prompt 要求写名词短语清单 |
| P3 | `confidence=observed` 装推断 | "响应缺 X-Powered-By" ⇒ "/login 是 nginx 静态文件"标成 observed | 在工具描述里钉死两档语义（缺失/差异推出的一律 inferred） |
| P4 | 单位成本 | 同一个小站 176k 输入 / 2 条事实；单意图 20+ 工具调用 | 意图级预算（P1-4）+ http 响应两档返回 + worker 可见 `list_facts` 防重复探测 |

**本轮 P0 修的是"线缆上的浪费"，不是"token 成本"**：请求从 45 降到 25、失败率 49%→0%，
但这批基准的总输入 token 154k→177k（因为 planner 多了一轮、worker 一次也没撞到错端口而把
轮次花在了真探测上）。成本要单独按 P4 处理，别指望 P0 顺带省钱。
