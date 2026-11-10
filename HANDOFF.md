# 钦原 · 交接文档（写给下一个接手的人 / AI）

> **读法**：本文是"当前真实状态"的唯一入口。先读 §0–§3 建立心智模型，再按需读 §4–§9。
> §11（已修缺陷）和 §14（常见坑）是**最省时间的两节**——里面每一条都是真实踩过的。
> 如果你只想知道"能不能跑"，直接看 §2 的复制粘贴命令。
>
> 文档时间点：M3a 完成、`-race` 打通之后。代码以 `git log` 为准，不以本文为准。

---

## 0. 给接手 AI 的五条纪律

1. **不要重写架构**。当前的包划分、CAS 队列、单写者、状态机位置都是刻意的，理由见 §4/§5。
2. **状态只能在 DB 里**。内存 map 只是"活着的进程状态"，丢了必须能从 DB 重建（唯一例外见 §14.7）。
3. **所有状态迁移走 CAS**（带 expected），禁止"读出来判断再写回去"。
4. **边界靠工具面，不靠 prompt 说教**。worker 拿不到 `add_intent`/`prove_goal`，这比写十句"你别越界"有效。
5. **动手前先问**。项目所有者在 M3 结束时明确要求"停一轮等 review"；新功能（M3.5/M4…）必须先确认。

---

## 1. 这是什么 / 做到哪了

**一句话**：钦原是一个**授权 Web 安全测试 Agent**：把探索"思考"外化成一张可查询、可审计、可重启的图（`nodes/edges/anchors`），再用事件驱动的调度器让 LLM 在图上反复"规划 → 执行 → 写回"。

- 语言：Go（后端全部）+ PostgreSQL（唯一存储）
- 定位：clean-room 实现，**不包含** AGPL 的 ARTEX / norma 源码；许可证 MIT
- 参考文档：`E:\项目\项目学习\{学习.md, ARTEX-架构笔记.md, 升级计划.md, 实现规格\}`（**只读参考，不要拷贝 ARTEX 代码**）

### 进度矩阵

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M0 | 幂等迁移、7 张核心表、advisory lock | ✅ |
| M1 | CAS 领取、worker 执行循环、工具层、九分支收尾、CLI/API、demo | ✅ |
| M2 | planner 主链（notify/debounce/心跳/六闸门）、OpenAI 兼容 provider、超时收尾、启动恢复 | ✅ |
| M3a | 计划图（plan/step + depends_on + expected_evidence 规则判定）、覆盖度、off_plan 逃生门 | ✅ |
| M3.5 | goals 拆解 agent、差分态势（P1-2） | ❌ 未做 |
| M4 | 流式 + settlement + compaction + steer_work + 删除流水线 + 准入队列 | ❌ 未做 |
| M5 | activity SSE + Next.js 静态导出 + 链路图 | ❌ 未做 |
| M6 | 审批门 + 流量录制 + finding 复测 + 报告 | ❌ 未做 |

> 当前**没有前端**：只有 JSON API + CLI。`activity` 表已经按前端需要落库（含 `tool_use_id` 配对）。

---

## 2. 环境与启动（复制即可）

### 2.1 已安装的东西（不要重复安装）

| 组件 | 路径 / 版本 | 说明 |
|---|---|---|
| Go | `C:\Program Files\Go`，`go1.26.2 windows/amd64` | 系统 PATH 已更新，但**旧进程**里可能还是旧 PATH；用 `scripts/go.ps1` 最稳 |
| GCC | `E:\toolchains\mingw64\bin\gcc.exe`，MinGW-W64 16.2.0 | `-race` 需要；PATH 里另有 SageMath gcc 10.2，**不要用**（与 Go race runtime 不兼容） |
| PostgreSQL | `%LOCALAPPDATA%\qinyuan\pg\pgsql`（17.6 便携版） | 数据目录 `%LOCALAPPDATA%\qinyuan\data`；端口 **55432**；用户 `postgres`；trust 认证 |
| 数据库 | `qinyuan`（开发）/ `qinyuan_test`（测试） | 测试每个用例建独立 schema，跑完 drop |

**为什么 PG 不在项目目录里**：`initdb` 在 Windows 上无法处理含中文的数据目录（实测 `FATAL: invalid byte sequence ... 0xcf 0xee`）。项目路径是 `E:\项目\项目学习\钦原`，所以整个 PG 运行时放到了 ASCII 路径。

### 2.2 启动数据库（当前可能已被杀掉，先检查）

```powershell
# 检查
& "$env:LOCALAPPDATA\qinyuan\pg\pgsql\bin\pg_isready.exe" -h 127.0.0.1 -p 55432

# 没起就前台跑（另开一个终端；Ctrl+C 停）
pwsh -File scripts/devdb.ps1 serve
```

> **重要**：`scripts/devdb.ps1 start` 在本环境（DSH 的 Windows Job 对象）里会"命令不返回"或者 DB 随调用方被杀。**用 `serve`**，或者像本次会话一样把 `postgres.exe` 挂在常驻后台任务里。会话间隔较长时后台任务可能被终止（退出码 `1073807364`），重启即可；非正常关闭后 PG 会自动 WAL 恢复，数据不丢。

### 2.3 跑起来

```powershell
pwsh -File scripts/go.ps1 migrate                 # 幂等迁移（可反复执行）
pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo  # 端到端自检，不联网
pwsh -File scripts/go.ps1 run ./cmd/qinyuan serve # 127.0.0.1:8787
```

`demo` 成功时最后一行：

```
✓ demo 通过：范围闸门 → HTTP 探测 → 实体/事实写回 → 意图 done → 配对完整
```

### 2.4 跑测试（含竞态）

```powershell
pwsh -File scripts/go.ps1 test ./... -count=1
pwsh -File scripts/race.ps1 ./...            # 全量 -race（约 40s，当前零数据竞争）
pwsh -File scripts/race.ps1 ./internal/store/... ./internal/engine/...
```

测试库不可用时，`store`/`engine`/`agent` 的集成测试会 **skip** 而不是失败（`testkit` 里 `t.Skipf`）。

### 2.5 接真实模型

```powershell
# .env.local（已在 .gitignore；不要提交）
QINYUAN_LLM_PROVIDER=openai
QINYUAN_LLM_BASE_URL=https://api.deepseek.com/v1   # 或任何 OpenAI Chat Completions 兼容端点
QINYUAN_LLM_API_KEY=sk-...
QINYUAN_LLM_MODEL=deepseek-chat
```

启动日志会打印 `provider=openai model=... base_url=... key=***xxxx`（只露末 4 位）。key 不入库、不进日志。

### 2.6 全部配置项

优先级：**ENV > `.env.local` > 默认值**（`internal/config` 会返回来源，`migrate` 会打印 DSN 来源）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `QINYUAN_DSN` | `postgres://postgres@127.0.0.1:55432/qinyuan?sslmode=disable` | |
| `QINYUAN_DATA_DIR` | `./data`（转绝对路径） | 工具大输出落盘目录 |
| `QINYUAN_ADDR` | `127.0.0.1:8787` | HTTP 监听 |
| `QINYUAN_ALLOW_LOOPBACK` | `0` | 允许 HTTP 工具访问回环；**只在 demo/自测开** |
| `QINYUAN_MAX_TOOL_OUTPUT` | `30000` | 单次工具输出上限（字符），超出落盘 |
| `QINYUAN_MAX_TURNS` | `12` | worker 每次执行回合上限 |
| `QINYUAN_TIMEOUT_SECONDS` | `600` | worker 每次执行时长上限 |
| `QINYUAN_LLM_PROVIDER` | `fake` | `fake` / `openai` |
| `QINYUAN_LLM_BASE_URL` | `https://api.openai.com/v1` | |
| `QINYUAN_LLM_API_KEY` | 空 | |
| `QINYUAN_LLM_MODEL` | 空 | openai provider 必填 |
| `QINYUAN_LLM_MAX_TOKENS` | `4096` | |
| `QINYUAN_LLM_TEMPERATURE` | `0.2` | |
| `QINYUAN_LLM_TIMEOUT_SECONDS` | `120` | |
| `QINYUAN_LLM_MAX_RETRIES` | `2` | 仅对 429/5xx/网络重试 |

---

## 3. 仓库结构

```
钦原/
├── HANDOFF.md                 ← 本文
├── README.md                  快速开始（给人）
├── docs/
│   ├── 00-设计定稿.md          决策 / 不变量 / 偏离 / 验收对照
│   ├── 01-M2-规划者与模型接入.md
│   └── 02-M3-计划图与覆盖度.md
├── scripts/
│   ├── devdb.ps1              便携 PG：init/start/stop/status/serve/psql/reset
│   ├── go.ps1                 64 位 Go（系统优先，便携兜底）
│   └── race.ps1               -race（自动选可用 gcc）
├── cmd/qinyuan/main.go        CLI 与全量接线（唯一知道"命令"的地方）
└── internal/
    ├── config/                配置装配（ENV/.env/默认 + 来源 + 打码）
    ├── model/                 纯类型 + DeriveStatus（全站唯一状态推导）
    ├── store/                 唯一数据访问层（pgx）
    │   ├── schema.sql         幂等 DDL（重启即迁移）
    │   ├── store.go           连接池 / 错误 / Open
    │   ├── migrate.go         advisory lock + 多语句执行
    │   ├── runs.go            run/run_tasks/scope/constraints
    │   ├── graph.go           nodes/edges：CAS、intent/fact/finding、版本号
    │   ├── entities.go        实体 upsert（部分唯一索引去重）
    │   ├── activity.go        过程库（只追加 + 清洗 + token 口径）
    │   ├── goals.go           目标 + ProveGoal
    │   ├── plans.go           计划图 + ready/done 判定 + expectedMatches
    │   └── coverage.go        覆盖度（只认 fact 锚点）
    ├── domain/                归一化（host/URL/eTLD+1/网段）+ 范围闸门
    ├── llm/                   Provider 接口 + Fake/Dynamic + OpenAI 兼容适配
    ├── agent/                 prompt、工具面、worker/planner 执行循环
    ├── engine/                调度：循环、闸门、收尾、超时、恢复、暂停
    ├── runplan/               用户输入 → 建 run 计划（范围 + 种子实体）
    ├── server/                HTTP 入口（解析 + DTO）
    └── testkit/               测试隔离库与夹具（只被 *_test.go 用）
```

### 依赖规则（硬性，由 import 图约束）

| 规则 | 现状 |
|---|---|
| `store` 不导入任何上层 | ✅ 只依赖 `model` + pgx |
| `agent` 不导入 `engine` | ✅ |
| `engine` 可以导入 `agent` + `store` | ✅ |
| 工具不直接依赖编排层（回调注入） | ✅ 工具只拿 `agent.Context` + `Emitter` |
| 前端只走 REST/SSE | M5 才有前端 |

### 代码规模（约）

生产 Go ≈ 7.5k 行、测试 ≈ 1.9k 行、schema ≈ 300 行。39 个测试。

---

## 4. 数据模型（`internal/store/schema.sql`）

### 4.1 表

| 表 | 作用 | 关键点 |
|---|---|---|
| `runs` | 一次探索 | `status ∈ open/achieved/failed`（知识层），`version` 写入口自增，`round_no` 冷节点/轮次基准 |
| `run_tasks` | 业务状态 | `status ∈ created/running/paused/done/failed/timeout`；`paused/queued/timeout_seconds/first_run_at/deadline_at/worker_count/heartbeat_seconds` |
| `nodes` | 图节点 | `kind ∈ goal/intent/fact/finding/hint/digest/plan/step`；状态机写在 `ck_node_state` CHECK 里 |
| `edges` | 边 | `rel ∈ spawns/derived_from/yields/proves/covers/satisfies/realized_by/depends_on`；`src=上游, dst=下游` |
| `anchors` | 探索节点 ↔ 实体 | 覆盖度的唯一依据 |
| `activity` | 过程库 | **只追加**；`tool_use_id` 配对；token 只认 `kind='result'` |
| `entities` | 领域实体（跨 run 共享） | 6 种 kind，每种一条**部分唯一索引**；`run_ids BIGINT[]` |
| `run_scope` | 授权范围 / 覆盖度分母 | `kind ∈ org/root_domain/subdomain/ip/cidr/icp/keyword` |
| `constraints` | 操作约束 | `allow/deny`，注入 prompt |
| `orgs/org_scope` | 多租户归属 | M1 建表，重算未接（M6/P3） |
| `settings` | 迁移旗标/偏好 | |
| `findings` | **视图**（不是表） | `nodes(kind='finding')` 是唯一真相源 |

### 4.2 三个有意的偏离（不要"修正"回去）

1. **`findings` 是视图**。ARTEX 的双写（节点 payload + findings 表）会静默不一致；钦原只存一份。
   代价：`list_findings` 的 `id` 与 `finding_id` 同源（工具描述里已写清）。
   以后要加 PoC 验证，请新建 `finding_verifications` 表，**不要**在 finding 上开第二份副本。
2. **`entities.auth/params/open_ports` 用 `JSONB` 数组**（不是 `JSONB[]`），合并语句更简单。
3. **M2 只有一个种子目标**（= 用户目标）。M1 建的历史 run 由 schema 末尾的幂等 `INSERT` 回填。
   多目标拆分是 M3.5 的 goals agent。

### 4.3 去重与归一化

- 去重**完全交给 DB**：每种实体一条部分唯一索引；`ON CONFLICT` 的 target 与索引谓词**逐字一致**（含 `COALESCE(...)`）。
- 归一化在 `internal/domain`：小写、去尾点、eTLD+1、`/24`/`/48`、`service_name` 小写。
- 合并语义统一"只增不减 + COALESCE 保旧"：数组 `DISTINCT unnest(a||b)`，标量 `COALESCE(new, old)`，每行刷 `last_seen`。

### 4.4 迁移纪律

`schema.sql` **每次启动全量执行**，所以每一句都必须幂等：

- 加列：`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`；
- 约束升级：`DO $$ IF EXISTS(... pg_get_constraintdef NOT LIKE '%新值%') THEN DROP/ADD $$`（M3 的 step/plan 就是这么加的）；
- 一次性回填：`DO $$ IF NOT EXISTS (settings 旗标) $$`，否则每次启动都会重跑；
- 整个迁移在 `pg_advisory_lock` 下串行执行（`migrate.go`），多实例同时启动只有一个跑 DDL。

---

## 5. 运行时（`internal/engine/engine.go`）

### 5.1 每个 run 的 goroutine

```
1 × plannerLoop   （事件驱动：notify / heartbeat）
N × workerLoop    （拉取式：claim / sleep）
1 × deadlineLoop  （仅 timeout_seconds > 0；**不挂在 rt.wg 上**，见 §14.4）
```

所有循环的生命周期由引擎自己持有：`Run` 里用 `context.WithoutCancel(ctx)` 建 runtime ctx
（**不能挂 HTTP 请求的 ctx**，否则响应一返回任务就"启动即停止"）。退出用 `StopAll(ErrShutdown)`。

### 5.2 plannerLoop

```
等 notify（图变了）或 heartbeat（max(run.heartbeat_seconds, 600s)）
 → debounce（默认 800ms，合并突发变更）
 → 六道闸门（顺序固定）：
    1 LLM 就绪?  否 → 结束循环
    2 用户暂停?  是 → 结束循环（resume 会重建）
    3 正在删除?  M2 无删除流水线
    4 终态?      是 → 结束
    5 收尾中?    是 → 结束（终局轮由协调器直接调）
    6 还有 open goal?  否 → goalless 分支
 → 登记 inflight、StampFirstRun、写 round marker
 → drainTriggers → planner.Plan（每轮全新会话）
 → 若 res.Met：CAS 任务 done + shutdownRuntime(ErrGoalMet)
```

**goalless 分支**：frontier 抽干 → 任务 done + `shutdownRuntime`；否则继续等。
（如果只改状态不停循环，worker 会一直空转——这是 M2 实测到的 bug。）

### 5.3 workerLoop

```
Work():
  读 run → 暂停/终态/收尾中 → return false
  claimNext(Frontier(20) 逐条 CAS)
  StampFirstRun + CAS created/queued → running
  注册 workCtx（单条意图可被 kill/pause）
  worker.Execute(...) → 采样 cause → settle → 发 system activity
  Notify + trigger（finding / done）
```

**不要因为"此刻没有 open 意图"让 workerLoop 退出**：planner 可能正在产意图，提前退出会让任务永久停摆。

### 5.4 状态机

**任务**：`created → running ⇄ paused`，`→ done / failed / timeout`（均 CAS，且只写入者赢）。

**意图**（九分支收尾，顺序不能改，`TestSettleMapping` 锁住）：

| 顺序 | 条件 | 落点 |
|---|---|---|
| 1 | `paused_by_user` | `open`（放回 frontier，恢复后续跑） |
| 2 | `cancelled_by_user` | `paused`（先当栅栏，API 再清理） |
| 3 | `killed_by_planner` | `stopped` |
| 4 | `timeout_settling` | `exhausted` |
| 5 | `goal_met` | `stopped` |
| 6 | `shutdown` | `open` |
| 7 | 任务已暂停 | `open` |
| 8 | 任务已终态 | `stopped` |
| 9 | 执行报错 | `blocked`（LLM 链耗尽另标 `quota`） |
| 10 | 撞回合/时长上限 | `exhausted` |
| 11 | 其他 | `done` |

**具名取消原因**（`errors.New`，不是裸 `context.Canceled`）：
`paused_by_user / cancelled_by_user / killed_by_planner / timeout_settling / goal_met / shutdown`。
`cause` 必须在 `cancel(nil)` **之前**采样，否则每个正常结束都会被标成 stopped。

**计划步骤**：`todo → ready → running → done`（`failed/skipped` 预留）。

### 5.5 超时收尾（settleTask）

```
到点 → settling=true（停派/停领）
 → 等在跑 worker 收尾（SettleDrainGrace，默认 90s，计 inflight==0）
 → StopTask(ErrTimeoutSettling)（意图归 exhausted）
 → **换成 context.WithoutCancel** 再等 3s（否则 StopTask 取消后 sleep 立刻返回，收尾停在半路）
 → 终局 planner.Plan(final=true)：只判定目标、不派新意图
 → CAS timeout
```

### 5.6 通知与触发

- `notifyChan` 容量 1，`Notify` 非阻塞发送（**故意丢信号**：有变化不需要计数）。
- `triggers` 按 run 累积，`drainTriggers` 一次性消费，渲染成 planner 的【本次触发本轮的实际变动】。
- 触发种类：`done / finding / goal / goal_edited / goal_deleted / hint / cancelled / heartbeat`。
- 目前只有 `done`、`finding` 由 worker 产生；其它留给 API/人工路径（`AddTrigger` 已就绪，未接 UI）。

### 5.7 启动恢复

```
ResetRunningIntents()         # running → open（没有活着的 worker 拥有它们）
每个非终态 run：
  paused → 只记暂停（等用户 resume）
  queued → 跳过（M4 接准入队列）
  其他   → Run() 重建 planner + workers (+ deadline)
```

`serve` 启动时自动执行；`work` 不会自动恢复（避免误启动一堆任务）。

### 5.8 引擎默认值（可调，但要知道影响）

| 常量 | 值 | 影响 |
|---|---|---|
| `IdleSleep` | 500ms | worker 空转轮询 |
| `Debounce` | 800ms | 小 → 频繁规划烧钱；大 → 反应慢 |
| `HeartbeatFloor` | 600s | 纯事件驱动会死锁，心跳是兜底 |
| `SettleDrainGrace` | 90s | 收尾等在跑 worker 的上限 |
| `FinalPlanTimeout` | 2min | 终局判定独立 ctx |
| `MaxTurns` | 12 | worker 每次执行回合上限 |
| `MaxDuration` | 10min | worker 每次执行时长上限 |

---

## 6. 工具面（模型的动作空间）

### 6.1 矩阵

| 工具 | planner | worker | 说明 |
|---|:--:|:--:|---|
| `overview` | ✅ | — | 与 planner 预取**同一份代码** |
| `list_facts` | ✅ | — | 只给摘要 |
| `list_findings` | ✅ | ✅ | 上报前查重 |
| `node_detail` | ✅ | ✅ | 唯一拿 evidence 全文的口子 |
| `get_worker_output` | ✅ | — | 判断意图"做到哪、卡在哪" |
| `add_plan_step` | ✅ | — | 必须有 `expected_evidence` |
| `list_plan_steps` | ✅ | — | 计划全貌 |
| `mark_step_done` | ✅ | — | 规则没命中时显式判定 |
| `add_intent` | ✅ | — | 有计划图时必须挂 `step_id` |
| `prove_goal` | ✅ | — | 唯一能让任务正常结束的路径之一 |
| `kill_work` | ✅ | — | 止损 |
| `http_request` | — | ✅ | 范围闸门 + 出网守卫 |
| `insert_assets` | — | ✅ | 只登记实体，不写结论 |
| `record_fact` | — | ✅ | evidence 必填；自动满足计划步骤 |
| `report_finding` | — | ✅ | 无证据一律拒绝 |

**故意的缺失**：worker 没有 `add_intent/prove_goal/add_scope/list_facts`；planner 没有 `http_request/insert_assets/record_fact`。边界靠工具面，不靠 prompt。

### 6.2 工具定义与硬规矩

```go
type Tool struct {
    Name, Description string
    Schema json.RawMessage
    ReadOnly bool
    Handle func(ctx, *Context, json.RawMessage) (Result, error)
}
```

- 每个 handler 外层 `recover()`：工具跑在独立 goroutine 上，panic 会带走整个进程；
- `store == nil` 返回**错误文本**，不 panic；
- 返回给模型的结果必须**结构化**（`{id,...}`），模型下一步要靠 id 链式操作；
- 大输出（> `MaxOutput`）落盘 `data/runs/<run>/cmd-output/<sha1>.txt`，只回 head + 指针；
- 截断必须带"怎么取全"的提示，否则模型会基于残缺信息决策。

### 6.3 几个工具的语义细节

- **`add_intent`**：`parent_ids` 只能是已存在的 fact/finding（否则整条拒绝）；无父自动连 origin fact（血缘不断）；**有计划图时 `step_id` 必填**，计划外探索用 `off_plan=true`；资产过范围闸门。
- **`prove_goal`**：goal 必须 `open`；证据必须是 fact/finding；**origin fact 不能当证据**；重复证明被拒。
- **`record_fact`**：写回后立刻做 `SatisfyStepsByFact`（规则命中 → 步骤 done → 下一步 ready），返回里带 `satisfied_steps`。
- **`http_request`**：只允许 http/https；范围闸门在发请求**之前**；拨号层拒绝 loopback/链路本地/组播/未指定（**不拒 RFC1918**）；跨主机重定向拒绝；响应最多读 4MB。
- **`insert_assets`**：逐条部分成功；越界条目单独报错；同一 URL+method 重复提交幂等。

---

## 7. 模型接入（`internal/llm`）

### 7.1 接口

```go
type Provider interface { Complete(ctx, *Request) (*Response, error) }
type Factory interface  { For(sessionKey string) Provider }
```

- `Request`：`System` + `Messages` + `Tools`（非流式整块返回）；
- `Response`：`Text/Thinking/ToolCalls/Usage/StopReason`；
- **会话键**：planner = `run<id>-planner`，worker = `run<id>-intent<id>`（按意图，不按槽位）。

三个实现：`FakeFactory`（脚本化，测试/空跑）、`DynamicFactory`（函数式，测试里可读请求再决定响应）、`OpenAI`（真实）。

### 7.2 OpenAI 适配

- 端点：`{base_url}/chat/completions`；`Authorization: Bearer <key>`；
- 工具：`tools[{type:function,function:{name,description,parameters}}]` + `tool_choice=auto`；
- 消息映射：assistant 的 `tool_calls`、tool 的 `tool_call_id/name/content`；
- usage：`prompt_tokens/completion_tokens/prompt_tokens_details.cached_tokens`；
- 工具参数不是合法 JSON 时，传 `{}` 给工具层，由工具报"参数解析失败"（不 panic）。

### 7.3 错误分类（决定"能不能换人/要不要退避"）

| 情况 | 分类 | 行为 |
|---|---|---|
| 401/403 | `ErrAuth` | 不重试；任务 blocked（quota 类标 `blocked_reason`） |
| 429 + 余额/配额关键词 | `ErrQuota` | 不重试（重试没用），交给上层换人 |
| 429 普通限流 | `ErrRateLimited` | 指数退避重试 |
| 5xx / 网络 / 408 | `ErrTransient` | 指数退避重试 |
| 400/404 | `ErrBadRequest` | 不重试 |

重试只覆盖"还没交付任何内容之前"的失败（L1/L2）。**当前是非流式**，不存在"吐了一半再换 provider"的问题；M4 引入流式后才需要 committed 边界。

> 配额识别的关键词刻意排除 `rate limit / rpm / tpm`，避免把普通限流误判成余额问题。

---

## 8. 安全边界（默认拒绝）

| 边界 | 实现 | 默认值 |
|---|---|---|
| 授权范围 | `run_scope` + `domain.ScopeSet`，工具**写入前**检查 | **空范围 = 拒绝一切** |
| 出网 | 自定义 `DialContext` 只连通过校验的 IP | 回环/链路本地/组播拒绝 |
| 内网 | **不拒绝 RFC1918**（内网自建服务是合法用法） | 刻意取舍，写在文档里 |
| 重定向 | 只允许同主机；≥5 次拒绝 | 凭据常在 URL 里 |
| 证据门槛 | fact 必须有 evidence；finding 必须有可复现证据 | 硬约束在 store |
| 破坏性操作 | M3a 的 HTTP 工具只做单请求，不提供批量爆破/write 原语 | M6 审批门再扩展 |

**范围匹配语义**（容易踩）：

- `root_domain`：匹配自身**和子域**（`*.example.com`）；
- `subdomain`：**精确匹配**（`api.example.com` 不覆盖 `x.api.example.com`）；
- `cidr/ip`：按 IP 匹配；
- 实体是 IP 时走 `AllowsIP`，不会因为 subdomain 规则里有同名字符串就放行。

`QINYUAN_ALLOW_LOOPBACK=1` 只在 demo/自测开，它会削弱 SSRF 防护。

---

## 9. 可观测与记账

- **单一发射点**：`engine.emit` 落库（3 次重试）成功后才算数；失败必打日志（含 run/kind/tool/tool_use_id）。
- **activity 只追加**；写入前做 UTF-8 清洗 + 去 NUL（否则 PG 整条拒收，表现是"某一步凭空消失"）。
- **tool_use ↔ tool_result 靠 `tool_use_id` 配对**，不能按相邻行（工具是并行的）。
- **token 只认 `kind='result'`**：`TokenTotals` 只 SUM 这一种行；`usage` 帧只做兜底。
- `activity.detail` 懒加载（列表不取）；API 目前只返回 summary。
- 大输出落盘路径可预测、幂等（同内容同 sha1）。
- 日志分级：`[engine] / [planner] / [worker] / [activity] / [llm] / [http]`。

---

## 10. 测试（39 个，`*_test.go`）

### 10.1 基础设施

- 每个测试一个**独立 schema**（`testkit.Store` / `store.newTestStore`），跑真实迁移，结束 drop；
- 测试库默认 `qinyuan_test`（可用 `QINYUAN_TEST_DSN` 覆盖）；
- 假 provider：`FakeFactory`（脚本）/ `DynamicFactory`（函数式）；
- `-race`：`scripts/race.ps1`，当前全绿。

### 10.2 覆盖清单（按包）

| 包 | 用例 | 守的是什么 |
|---|---|---|
| store | `TestClaimConcurrentNoDuplicates` | 60 条 / 10 worker，领取不重复（I1/I2） |
| store | `TestCASTransitionRejectsWrongExpected` | 错误 expected 不得覆盖 |
| store | `TestAddIntentLineage` | 顶层意图连 origin；非法父整条拒绝且不留孤儿 |
| store | `TestUpsertEntityIdempotent` | 幂等 + 合并语义 + run_ids 去重 |
| store | `TestActivitySanitizesNUL` | NUL/非法 UTF-8 不丢行 |
| store | `TestResetRunningIntents` | 崩溃残留 running → open |
| store | `TestProveGoalRejectsOriginFact` / `...IntentEvidence` | 证据语义 |
| store | `TestPlanChainReadyByDependency` | 5 步链只有 1 步 ready；事实命中自动解锁下一步 |
| store | `TestMarkStepDone` | 显式判定 + 证据类型校验 |
| store | `TestCoverageCountsOnlyFacts` | 只认 fact 锚点，origin 不算 |
| agent | `TestHTTPToolScopeGate` / `NoScopeDenies` / `LoopbackBlocked` / `Allowed` | 范围闸门 + SSRF 边界 |
| agent | `TestInsertAssetsPartialSuccessAndIdempotent` | 部分成功 + 幂等 |
| agent | `TestEvidenceGate` | 无证据 fact/finding 被拒 |
| agent | `TestAddIntentScopeGate` | 越界资产不建节点 |
| agent | `TestPlanStepAutoSatisfyAndIntentGate` | 自动满足 + step_id 强制 + off_plan 逃生门 |
| domain | `TestNormalizeHostAndRootDomain` / `NormalizeURL` / `SegmentFor` / `ScopeSet` / `SplitHostPort` / `DialableIP` | 归一化与范围匹配（按点边界） |
| engine | `TestWorkWritesFactAndDone` | 写回 + done + 配对 + 记账 |
| engine | `TestMaxTurnsExhausted` | 撞上限 ≠ 完成，且已写回的事实不丢 |
| engine | `TestSettleMapping`（8 子用例） | 九分支收尾顺序 |
| engine | `TestPausedDoesNotClaim` / `TestPauseDuringExecutionReflowsOpen` | 暂停拦新活 + 打断在跑 |
| engine | `TestPlannerWorkerGoalLoop` | planner 派意图 → worker 写事实 → prove_goal → done |
| engine | `TestGoallessBranchMarksDoneWithoutLLM` | 无 open goal 直接 done，**0 次模型调用** |
| engine | `TestRestoreResetsRunningAndContinues` | 恢复后继续推进 |
| engine | `TestDeadlineSettlesTimeout` | 超时收尾落 timeout |
| engine | `TestPlannerPlanStepChainEndToEnd` | 拆步 → 挂步派意图 → 规则命中 → prove → done（M3 全链路） |
| llm | `TestOpenAICompleteMapsTools` / `ErrorClassification` / `LooksLikeQuota` / `RetriesTransientThenGivesUp` | 请求/响应映射 + 错误分类 + 重试上限 |

### 10.3 尚未覆盖（要在对应里程碑补）

T4 删除屏障、T5 完整重启恢复（只有部分）、T6 准入队列、T13–T15 provider failover、T18–T20 SSE、T22–T24 审批竞态。

---

## 11. 已修缺陷清单（**不要踩回去**）

这些都是测试/冒烟真实抓出来的，每一条都值得先看再改相关代码。

| # | 现象 | 根因 | 修法 / 现在的约束 |
|---|---|---|---|
| 1 | 暂停后意图永远卡在 `running` | `settle` 用**已取消的 ctx** 写库 | 收尾用 `context.WithoutCancel` + 5s 超时 |
| 2 | 直接调 `Work`（HTTP `/work`、CLI `--once`）时暂停取消不到在跑的 ctx | `Pause` 只取消 taskCtx，而 `Work` 没有 task 层 | `workRuns` 映射 + `Pause` 取消该 run 所有在跑 work；注册后再查一次 paused |
| 3 | `start` 后一轮规划都不跑 | 引擎循环挂在 **HTTP 请求 ctx** 上 | `Run` 用 `WithoutCancel` 建 runtime ctx；退出用 `StopAll` |
| 4 | goalless 到 done 后 worker 一直空转 | 只改任务状态，没停 runtime | goalless 分支 `shutdownRuntime` |
| 5 | 目标被 origin fact「一证就达成」 | `ProveGoal` 没排除任务起点 | origin fact 明确拒绝 |
| 6 | 超时收尾停在半路，永远不落 timeout | `StopTask` 取消 rctx 后，后续 sleep/CAS 还在用它 | 收尾后半段换 `WithoutCancel` |
| 7 | 暂停偶发失效（任务仍在跑） | `created→running` 的 CAS 把 `paused` 也算进 expected，覆盖了刚设上的暂停 | expected 只留 `created/queued` |
| 8 | worker 在 planner 还没产意图时退出循环 | 用"此刻没有 open 意图"判断退出 | workerLoop 只在 ctx 结束时退出 |
| 9 | 超时协调器自锁 | `settleTask → StopTask → wg.Wait()`，而 deadlineLoop 自己在 wg 里 | deadlineLoop 不进 `rt.wg` |
| 10 | 步骤状态按"最新在前"返回，测试/渲染读反 | `NodesByKind` 是 id DESC | `PlanGraph` 单独按 id ASC 查 |

> 共性：**上下文生命周期**（1/3/6）与**状态机竞态**（2/7/8/9）。只看代码看不出来，必须让测试真的跑起来。

---

## 12. 已知限制 / 未实现（按里程碑）

### M3.5

- **goals 拆解 agent**：仍然只有一个种子目标（= 用户目标）。需要第 0 轮 `set_goals/set_constraints/add_scope` 工具集 + `run_tasks.goals_at` 标记。
- **差分态势（P1-2）**：仍是每轮全量 overview。`runs.version` 已在写入口自增，但"快照 + 自某版本以来的事件流"没做。
- **计划修订**：`revision+1` 没有接口，`off_plan` 只被记录，没有汇总成"计划该改"的信号。
- **步骤预算**：`payload.budget` 预留，worker 还没按步骤预算夹逼。

### M4

- 非流式 → 流式（以及 committed 边界、settlement 收尾轮）；
- compaction / micro-compact；
- `steer_work`（planner 实时纠偏在跑的 work）：目前只能 `kill_work`；
- 删除流水线（屏障 → 排水 → 两阶段清理）；API 的 `stop` 现在明确返回 **501**，不假装停了；
- 准入队列（`queued` 字段已在库，FIFO 未实现）；
- 重启恢复的"完整版"（deadline 重建、暂存删除恢复）。

### M5 / M6

- SSE + 前端（Next.js 静态导出 + `go:embed`）；链路图/覆盖图；
- 审批门（规则 → LLM 判官 → 人工卡片）、流量录制（MITM）、finding 复测（PoC 可复现）、报告；
- 鉴权：`serve` 目前只监听 127.0.0.1，**没有 token**，是开发用。

---

## 13. 扩展指南（照着做，别自创一套）

### 13.1 加一个工具

1. 在 `internal/agent/tool_*.go` 里定义 `&Tool{Name, Description, Schema, Handle}`；
2. handler 第一行做参数解析 + 硬校验，返回 `Errorf(...)`（可操作文案）；
3. 需要 store 就通过 `rc.Store`，需要编排能力（kill/notify）用 `rc.KillWork` 之类的回调，**不要 import engine**；
4. 加进 `NewPlannerRegistry` 或 `NewWorkerRegistry`（想清楚它属于哪个角色）；
5. 写一个测试：至少覆盖"正常路径 + 一个拒绝路径"。

### 13.2 加一种节点 kind / 状态

1. `schema.sql`：用 `DO $$ ... pg_get_constraintdef NOT LIKE '%新kind%' $$` 重建 `ck_node_kind` 和 `ck_node_state`；
2. `internal/model`：加 `KindXxx` / `StateXxx` 常量；
3. 想清楚它属于哪张图的语义、有没有写工具、planner 的 overview 要不要展示；
4. 补迁移测试（`migrate` 跑两遍）。

### 13.3 加一个迁移

- 写进 `schema.sql` 末尾，**必须幂等**；
- 加列 `IF NOT EXISTS`；约束升级用 `DO $$` 检测；数据回填用 settings 旗标；
- 在已有开发库上跑 `go run ./cmd/qinyuan migrate` 验证，再跑全量测试（测试会从零建 schema，两条路径都要过）。

### 13.4 加一个 provider

1. 实现 `llm.Provider`（`Complete`）+ 一个 `Factory`；
2. 错误必须映射到 `ErrAuth/ErrQuota/ErrRateLimited/ErrTransient/ErrBadRequest`（上层靠 `errors.Is` 决策）；
3. `Complete` 里自己处理 L1/L2 重试，**不要**在已交付 token 后重试（流式时尤其）；
4. 在 `cmd/qinyuan/main.go:buildProviders` 里加分支；
5. 用 `httptest` 写映射测试（请求体 + 响应解析 + 每种错误分类）。

---

## 14. 常见坑与判断规则

1. **不要用裸 `context.Canceled`**：状态机需要知道"为什么停"。用 `WithCancelCause` + 具名 cause。
2. **`cause` 采样必须在 `cancel(nil)` 之前**，否则正常结束会被标成 stopped。
3. **收尾写库用 `WithoutCancel`**：暂停/超时正是 cancel 之后才收尾。
4. **`deadlineLoop` 不能进 `rt.wg`**：它自己会 `StopTask → wg.Wait()`，算进去就自锁。
5. **引擎循环不能挂请求 ctx**：HTTP 响应一返回就被取消。
6. **workerLoop 不要因"暂时没活"退出**：planner 可能正在产意图。
7. **唯一允许丢的内存状态**是"模型自己的便签"；钦原目前没有 todo store（计划图已替代），所以没有这个例外。
8. **origin fact 不是证据**：它是任务起点（目标 + 初始范围）。
9. **`subdomain` 范围是精确匹配**，要覆盖子域用 `root_domain`。
10. **`ON CONFLICT` 谓词必须与部分唯一索引逐字一致**（含 `COALESCE(...)`），否则 PG 报 `no unique or exclusion constraint matching`。
11. **`activity` 的 `summary` 截断不要截到不可解析**：要么给结构化字段，要么保留完整 JSON。
12. **PG 数据目录不能含中文**（Windows `initdb` 限制）；所以运行时在 `%LOCALAPPDATA%\qinyuan`。
13. **旧终端 PATH 可能还是旧 Go/旧 gcc**：用 `scripts/go.ps1` / `scripts/race.ps1`，它们显式指定二进制。
14. **后台 job 可能被会话间隔杀掉**：DB 掉了先 `pg_isready`，再 `devdb.ps1 serve`；WAL 会自动恢复。
15. **测试并发用例多跑几遍**：`go test ./internal/store -run TestClaimConcurrent -count=20`（`-race` 之外再加一道）。

---

## 15. 当前交付物与 git 状态

```
077f3d3 工具链：接入 mingw-w64，scripts/race.ps1 跑 -race；全量竞态检测通过
00250f4 工具链：scripts/go.ps1 改为优先系统 64 位 Go（amd64），便携版兜底
e3a7cde M3a: 计划图（step 依赖 + expected_evidence 规则判定）+ 覆盖度 + off_plan 逃生门；切换 64 位 Go
bb6b6de M2: planner 主链（事件唤醒/心跳/六闸门/add_intent/prove_goal）+ OpenAI 兼容 provider + 超时收尾 + 启动恢复
bcb933a 修复暂停路径：settle 用不可取消的 ctx 写库；Work 支持被 Pause 取消在跑的执行
394f5b4 M0/M1: 钦原骨架——幂等迁移、CAS 队列、工具层、worker 闭环、CLI/API 与验收测试
```

工作区干净；`docs/00–02` 是设计与验收记录；本文是状态快照。

**验证一切正常的三条命令**：

```powershell
pwsh -File scripts/go.ps1 test ./... -count=1     # 39 个测试全绿
pwsh -File scripts/race.ps1 ./...                 # 零数据竞争
pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo  # ✓ demo 通过
```

---

## 16. 需要人拍板的问题（接手 AI 不要自己决定）

1. **M3.5 还是 M4**：goals 拆解 + 差分态势（上下文成本）vs 长跑可靠性（settlement/compaction/删除）。
2. **真实模型 live 冒烟**：需要 `base_url + model`（key 走环境变量）；这一步能暴露 prompt 质量与工具调用质量，是 fake provider 测不出来的。
3. **`expectedMatches` 的匹配规则**（中文 2-gram + 60% 覆盖）是否够准；太松会假完成，太紧会一直等人。
4. **`add_intent` 强制 `step_id`** 是否会让 planner 为了过校验硬凑步骤。
5. **许可证**：当前 MIT；如果将来要复用 ARTEX/norma 的任何代码，必须先停下来讨论 AGPL 传染问题。
6. **生产形态**：PG 是继续便携实例还是独立实例；是否需要鉴权/多用户；部署目标（单二进制 + 反代）。
