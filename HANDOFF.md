# 钦原 · 交接文档（写给下一个接手的人 / AI）

> **读法**：本文是"当前真实状态"的唯一入口。先读 §0–§3 建立心智模型，再按需读 §4–§9。
> §11（已修缺陷）和 §14（常见坑）是**最省时间的两节**——里面每一条都是真实踩过的。
> 如果你只想知道"能不能跑"，直接看 §2 的复制粘贴命令。
>
> 文档时间点：M4a 完成（收尾轮 + 压缩 + 纠偏）、`-race` 全绿之后。代码以 `git log` 为准，不以本文为准。

---

## 0. 给接手 AI 的六条纪律

1. **不要重写架构**。当前的包划分、CAS 队列、单写者、状态机位置都是刻意的，理由见 §4/§5。
2. **状态只能在 DB 里**。内存 map 只是"活着的进程状态"，丢了必须能从 DB 重建（唯一例外见 §14.7）。
3. **所有状态迁移走 CAS**（带 expected），禁止"读出来判断再写回去"。
4. **边界靠工具面，不靠 prompt 说教**。worker 拿不到 `add_intent`/`prove_goal`，这比写十句"你别越界"有效。
5. **动手前先问**。项目所有者在 M3 结束时明确要求"停一轮等 review"；新功能（M4…）必须先确认。
6. **新增写路径必须打 `rev`**（M3.5 起）。`nodes`/`edges` 的每一行都靠 `rev` 参与差分态势：
   漏一条 = 那条变化从增量里静默消失 = planner 基于过时态势决策。清单见
   `docs/03-M3.5-目标拆解与差分态势.md §3.3`，`store/delta_test.go` 会逐边对账。

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
| M3.5 | 第 0 轮 goals 拆解（`set_goals`/`set_constraints`/`add_scope`）+ 差分态势（P1-2：`rev` 版本戳 / 增量输入 / `refresh_overview`） | ✅ |
| M4a | settlement 收尾轮 + compaction/micro-compact + `steer_work` 实时纠偏 | ✅ |
| M4b | 删除流水线（屏障→排水→两阶段）+ 准入队列（FIFO）+ 完整启动恢复 | ✅ |
| M4c | 流式输出（SSE）+ committed 边界（未提交才可重试） | ✅ |
| M5a | activity SSE + 仪表盘（手写 SPA + `go:embed`，IA 按 ARTEX；**不含链路图**，见 `docs/11 §6`） | ✅ |
| M5b | 链路图（自写分层布局）+ 覆盖图（同心圆 + 折叠） | ✅ |
| M6a | 审批门（规则 → 人工挂起；默认只对 DELETE/PUT/PATCH 要审批） | ✅ |
| M6b | 流量录制（工具层录制 + PG 索引 + 证据快照；**不做代理**，见 `docs/13 §1`） | ✅ |
| M6c | finding 复测（重放+断言，三档结论）+ Markdown 报告 | ✅ |
| M7 | 平台化七件套：trace / outbox / GraphStore+memstore / 战役 / 审批 SLA / 可复现报告 / 连接器（见 `docs/15`） | ✅（部分到 S1–S2，见该文 §0） |

> 控制台：`serve` 之后打开 `http://127.0.0.1:8787/` —— 侧边导航 + 卡片式仪表盘
> （仪表盘 / 任务 / 发现 / 资产 / 设置；任务详情五 Tab：概览 / 会话 / 计划 / 成本 / 覆盖）。
> 会话走任务级 SSE（`/api/runs/{id}/activity/stream`）；审批在「审批」页（侧边栏带 pending 红点）。

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
| `QINYUAN_GOALS_MAX_TURNS` | `8` | 第 0 轮目标拆解的回合上限（1..30） |
| `QINYUAN_PLANNER_FULL_EVERY` | `20` | planner 每 K 轮给一次全量态势（其余给增量） |
| `QINYUAN_DELTA_LIMIT` | `200` | 单轮增量最多多少条变化，超过退全量（10..5000） |
| `QINYUAN_SETTLEMENT_TURNS` | `3` | 撞预算后的收尾轮数（0 = 关闭） |
| `QINYUAN_SETTLEMENT_TIMEOUT_SECONDS` | `120` | 收尾轮独立预算（10..900） |
| `QINYUAN_CONTEXT_WINDOW_K` | `200` | 上下文窗口估计（K token）；0 = 关闭压缩 |
| `QINYUAN_MAX_CONCURRENT_RUNS` | `0` | 同时跑的任务上限；0 = 不限（1..100） |
| `QINYUAN_APPROVAL_LOW_SECONDS` / `_MID_` / `_HIGH_` | `60` / `300` / `900` | 按风险分级的审批等待预算（5..3600） |
| `QINYUAN_APPROVAL_{LOW,MID,HIGH}_ACTION` | `allow` / `deny` / `deny` | 分级超时动作 |
| `QINYUAN_CONNECTOR_ASSETS_URL` / `_TOKEN` | 空 | 资产回写端点（空 = 不启用连接器） |
| `QINYUAN_CONNECTOR_TICKET_URL` / `_TOKEN` | 空 | 工单端点（空 = 不启用） |

---

## 3. 仓库结构

```
钦原/
├── HANDOFF.md                 ← 本文
├── README.md                  快速开始（给人）
├── docs/
│   ├── 00-设计定稿.md          决策 / 不变量 / 偏离 / 验收对照
│   ├── 01-M2-规划者与模型接入.md
│   ├── 02-M3-计划图与覆盖度.md
│   ├── 03-M3.5-目标拆解与差分态势.md
│   ├── 04-M4a-撞预算不白跑.md
│   ├── 05-M4b-删除与准入.md
│   ├── 06-M4c-流式与committed.md
│   └── 15-M7-平台化七件套.md    trace / outbox / GraphStore / 战役 / 审批SLA / 报告 / 连接器
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
    │   ├── goals.go           目标 + ProveGoal + 拆解替换 / goals_at 标记
    │   ├── plans.go           计划图 + ready/done 判定 + expectedMatches
    │   ├── delta.go           版本分配（bumpVersion）+ 差分读取（rev > since）
    │   ├── tasks_lifecycle.go 删除级联 / 队列（Enqueue/NextQueued/CountActive）
    │   └── coverage.go        覆盖度（只认 fact 锚点）
    ├── domain/                归一化（host/URL/eTLD+1/网段）+ 范围闸门
    ├── llm/                   Provider 接口 + Fake/Dynamic + OpenAI 兼容（含 SSE 流式 + committed）
    ├── agent/                 prompt、工具面、worker/planner/goals 执行循环、增量态势
    ├── engine/                调度：循环、闸门、收尾、超时、恢复、暂停、删除、准入
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

生产 Go ≈ 10.4k 行、测试 ≈ 3.5k 行、schema ≈ 330 行。110 个测试。

---

## 4. 数据模型（`internal/store/schema.sql`）

### 4.1 表

| 表 | 作用 | 关键点 |
|---|---|---|
| `runs` | 一次探索 | `status ∈ open/achieved/failed`（知识层）；`version` 是**单调版本分配器**（差分量游标），`round_no` 轮次基准 |
| `run_tasks` | 业务状态 | `status ∈ created/running/paused/done/failed/timeout`；`paused/queued/timeout_seconds/first_run_at/deadline_at/worker_count/heartbeat_seconds`；**`goals_at`**=第 0 轮拆解完成时刻（0=未拆） |
| `nodes` | 图节点 | `kind ∈ goal/intent/fact/finding/hint/digest/plan/step`；状态机写在 `ck_node_state` CHECK 里；**`rev`**=最后一次写入的版本号（差分用） |
| `edges` | 边 | `rel ∈ spawns/derived_from/yields/proves/covers/satisfies/realized_by/depends_on`；`src=上游, dst=下游`；**`rev`** 同上 |
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
   4.5 goals_at==0? 是 → 第 0 轮目标拆解（只跑一次；失败不写标记、下一轮重试）
    6 还有 open goal?  否 → goalless 分支
 → 登记 inflight、StampFirstRun、写 round marker
 → drainTriggers → planner.PlanRound（每轮全新会话；决定给全量还是增量态势）
 → 若 res.Met：CAS 任务 done + shutdownRuntime(ErrGoalMet)
```

**4.5 必须排在 6 之前**：拆解还没跑时图里只有 seed 目标，先判 goalless 会把"还没拆解"误判成
"目标都已达成"（详见 §14.16）。

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
recoverStagedRemovals()       # .deleting/* 按 DB 真相收尾（无 → 真删；有 → 还原）
ResetRunningIntents()         # running → open（没有活着的 worker 拥有它们）
每个非终态 run：
  paused → 只记暂停（等用户 resume）
  queued → 跳过（末尾由 Reconcile 按 FIFO 补位）
  其他   → Run() 重建 planner + workers (+ deadline)
Reconcile()                   # 队列里的任务不会自己启动
```

`serve` 启动时自动执行；`work` 不会自动恢复（避免误启动一堆任务）。

### 5.7b 删除与准入（M4b）

```
删除（stop / DELETE）：
  BeginDelete（屏障：deleting + paused + 取消在跑 work）
  → StopTask（有上限）→ waitQuiescent（inflight==0）
  → data/runs/<id> rename 到 .deleting/<id>-<ts>（可逆）
  → DB：DELETE run_tasks → DELETE runs（级联）
  → 提交后才 RemoveAll(暂存) → forgetRun → Reconcile
  失败：DB 失败 → 还原文件 + 撤销屏障（任务保持 paused）；DB 成功文件失败 → 仍算成功（warning）

准入（启动一个任务）：
  limit<=0 → 直接 Run
  active = COUNT(status='running' AND !queued AND !paused)   ← created **不算**占坑
  active >= limit → EnqueueRun（queued_at 首次写入后不再改）+ 一条 system activity
  否则 → Dequeue + CAS created/queued→running + Run
  补位：任务结束/删除时同步一次 + ReconcileLoop(5s) 兜底
```

**`stop` 就是删除**（钦原没有"停机保号"状态）：想保留数据用 `pause`。

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
| `GoalsMaxTurns` | 8 | 第 0 轮拆解回合上限（`QINYUAN_GOALS_MAX_TURNS`） |
| `PlannerFullEvery` | 20 | 每 K 轮给一次全量态势（`QINYUAN_PLANNER_FULL_EVERY`） |
| `DeltaLimit` | 200 | 单轮增量上限，超过退全量（`QINYUAN_DELTA_LIMIT`） |
| `SettlementTurns` | 3 | 撞预算后的收尾轮数（`QINYUAN_SETTLEMENT_TURNS`，0=关） |
| `SettlementTimeout` | 2min | 收尾轮独立预算（`QINYUAN_SETTLEMENT_TIMEOUT_SECONDS`） |
| `ContextWindowTokens` | 200k | 压缩阈值用的窗口估计（`QINYUAN_CONTEXT_WINDOW_K`，0=关） |
| `MaxConcurrentRuns` | 0 | 同时跑的任务上限（`QINYUAN_MAX_CONCURRENT_RUNS`，0=不限） |
| `DeleteDrainTimeout` | 10s | 删除时等循环退出/静默的上限（安全阀，不进 ENV） |

---

## 6. 工具面（模型的动作空间）

### 6.1 矩阵

| 工具 | planner | worker | goals | 说明 |
|---|:--:|:--:|:--:|---|
| `overview` | ✅ | — | — | 与 planner 预取**同一份代码** |
| `refresh_overview` | ✅ | — | — | P1-2 逃生门：要全量（并让下一轮也全量） |
| `list_facts` | ✅ | — | — | 只给摘要 |
| `list_findings` | ✅ | ✅ | — | 上报前查重 |
| `node_detail` | ✅ | ✅ | — | 唯一拿 evidence 全文的口子 |
| `get_worker_output` | ✅ | — | — | 判断意图"做到哪、卡在哪" |
| `add_plan_step` | ✅ | — | — | 必须有 `expected_evidence` |
| `list_plan_steps` | ✅ | — | — | 计划全貌 |
| `mark_step_done` | ✅ | — | — | 规则没命中时显式判定 |
| `add_intent` | ✅ | — | — | 有计划图时必须挂 `step_id` |
| `prove_goal` | ✅ | — | — | 唯一能让任务正常结束的路径之一 |
| `kill_work` | ✅ | — | — | 止损 |
| `steer_work` | ✅ | — | — | 实时纠偏：下一次工具调用前生效并拦下它 |
| `http_request` | — | ✅ | — | 范围闸门 + 出网守卫 |
| `insert_assets` | — | ✅ | — | 只登记实体，不写结论 |
| `record_fact` | — | ✅ | — | evidence 必填；自动满足计划步骤 |
| `report_finding` | — | ✅ | — | 无证据一律拒绝 |
| `set_goals` | — | — | ✅ | 第 0 轮：替换 seed 目标 |
| `set_constraints` | — | — | ✅ | 第 0 轮：allow/deny（必须自包含） |
| `add_scope` | — | — | ✅ | 第 0 轮：最小授权范围 |

**故意的缺失**：worker 没有 `add_intent/prove_goal/add_scope/list_facts`；planner 没有 `http_request/insert_assets/record_fact`；
goals 只能登记目标/约束/范围（不派方向、不判完成、不干活）。边界靠工具面，不靠 prompt。

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

### 7.2b 流式（M4c）

- `llm.Streamer` 是**可选能力**：实现它就走 SSE（`stream:true` + `stream_options.include_usage`），
  不实现就继续整块 `Complete`（假 provider 走这条，行为不变）；
- 合并规则：`delta.content` 拼接、`delta.tool_calls` 按 `index` 合并、`arguments` 是**分片**要追加
  （整块覆盖会得到半个 JSON）；`reasoning_content` 进 `Thinking`；
- **committed 边界**：第一条内容/工具调用片之后不可重放。provider 自己负责——未 committed 才重试；
  已 committed 断流返回 **部分结果 + `ErrStreamInterrupted`**，worker 保留部分文本并落 `blocked`；
- 观测：worker 记 **TTFT**（首字延迟）→ 一条 system activity + result 的 `metadata.ttft_ms`；
- planner / goals 仍用整块调用（回合短，收益低，先不扩散改动面）。

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
- **态势形态可查**：planner 每轮的 result 行 `metadata` 带 `situation`(full|delta) / `changes` / `prompt_bytes`，
  "这一轮为什么贵"不用再猜；worker 取值新增 `goals`（第 0 轮拆解）。

---

## 10. 测试（110 个，`*_test.go`）

### 10.1 基础设施

- 每个测试一个**独立 schema**（`testkit.Store` / `store.newTestStore`），跑真实迁移，结束 drop；
- 测试库默认 `qinyuan_test`（可用 `QINYUAN_TEST_DSN` 覆盖）；
- 假 provider：`FakeFactory`（脚本）/ `DynamicFactory`（函数式）；
- 读调用次数用 `FakeFactory.CallCount(key)`（带锁）；直接读 `Calls[key]` 会和后台引擎写 map 竞争，`-race` 会抓到；
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
| store | `TestDeltaReconstructsFullGraph` | **增量重建 == 全量**：14 类写路径逐条走一遍，每步对账 |
| store | `TestDeltaTruncatesWhenTooManyChanges` | 变化超限 → Truncated 且不返回半截 |
| store | `TestDeltaWindowIsExclusive` | 窗口 `(since, to]`：不漏读、不重复读 |
| agent | `TestHTTPToolScopeGate` / `NoScopeDenies` / `LoopbackBlocked` / `Allowed` | 范围闸门 + SSRF 边界 |
| agent | `TestInsertAssetsPartialSuccessAndIdempotent` | 部分成功 + 幂等 |
| agent | `TestEvidenceGate` | 无证据 fact/finding 被拒 |
| agent | `TestAddIntentScopeGate` | 越界资产不建节点 |
| agent | `TestPlanStepAutoSatisfyAndIntentGate` | 自动满足 + step_id 强制 + off_plan 逃生门 |
| agent | `TestPlannerDeltaAfterFirstRound` | 首轮全量；第二轮增量且带出两轮之间新写的事实 |
| agent | `TestPlannerFullSnapshotEveryK` | 每 K 轮校准一次全量 |
| agent | `TestPlannerRefreshOverviewForcesFull` | `refresh_overview` 后下一轮回到全量 |
| agent | `TestDeltaPromptIsMuchSmaller` | 满规模图上增量 36%（−64%），守住 ≤60% |
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
| engine | `TestEnsureGoalsDecomposesOnce` | 拆解写目标/约束/范围；seed 目标退役；第二次 0 次模型调用 |
| engine | `TestEnsureGoalsFailureKeepsSeedGoal` | 失败不写 goals_at、seed 目标兜底、留 system 记录 |
| engine | `TestEngineGoalsRoundBeforePlanner` | 端到端：先拆解再规划（闸门顺序） |
| engine | `TestSettlementWritesBackOnMaxTurns` | 撞 `MaxTurns` → 收尾轮再写一条事实；终态仍 `exhausted`；收尾轮隐藏 `http_request` |
| engine | `TestSteerWorkBlocksNextToolCall` | 纠偏在下一次工具调用前生效、被拦下的写入没落库、模型看到纠偏文本 |
| agent | `TestCompactionNoOpUnderThreshold` / `MicrosOldToolResults` / `SummarizesAndKeepsPairing` / `SummarizerFailureStillCompacts` | 压缩只在需要时发生；不破坏 tool 配对；摘要失败不拖垮整轮 |
| agent | `TestEstimateTokensMonotonic` | 估计值随长度单调、CJK 至少一字一 token |
| engine | `TestDeleteRemovesRunAndFiles` | 删除后 run 与任务目录都不在；暂存清空；重复删除幂等 |
| engine | `TestDeleteWhileRunning` | 慢任务（5s 调用）能在 8s 内删掉，且 `inflight==0` |
| engine | `TestAdmissionQueueFIFO` | 上限 1 时 B 排队；A 删掉后 B 被补位 |
| engine | `TestRecoverStagedRemovals` | `.deleting` 目录按 DB 真相收尾（无→删；有→还原） |
| llm | `TestOpenAIStreamMergesDeltas` | 正文/thinking 拼接、tool_calls 按 index 合并、arguments 分片拼成合法 JSON、usage |
| llm | `TestOpenAIStreamInterruptedAfterCommitDoesNotRetry` | 断流 → `ErrStreamInterrupted` + 部分结果；请求数 = 1 |
| llm | `TestOpenAIStreamRetriesBeforeCommit` / `...QuotaNoRetry` | 未提交可重试；429 余额不重试且不泄凭据 |
| agent | `TestCompleteTurnUsesStreamerAndReportsTTFT` / `...FallsBackToComplete` | 有流式就走流式并报 TTFT；没有就退回整块 |
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
| 11 | `scripts/race.ps1` 把 `./...` 拆成 `.`、`/`、`.`… 然后报 "no Go files" | `$targets = if (...) { $args } else { @('./...') }`：PowerShell 的 if 表达式把**单元素数组解包成字符串**，随后 `@targets` 按字符展开 | 改成先赋数组再在分支里替换（见脚本注释）；`-race` 已重新全绿 |
| 12 | worker 不知道端口，49% 的 HTTP 请求打在 80/443/3000… | `runplan` 把 target 降成"范围"时把 scheme/port/path 全丢了，库里没有任何一处记着入口 URL | 新增 `runs.seeds` + 两个 prompt 各渲染一行"起点 URL"；详见 `docs/07` |
| 13 | 任务 `done` 后计划步骤永远停在 `running` | `LinkStepIntent` 只有 `ready/todo→running`，出口只有"规则命中"和"显式 mark"，意图结束没人回收 | `CASIntent` 落终态时同事务回收（`store/step_reconcile.go`）；`paused` 不回收 |
| 14 | 10 个真实步骤、0 次规则自动完成 | `expected_evidence` 是任务口吻整句，2-gram 覆盖率被动词稀释到 20~40%，且 2-gram 跨词边界 | `expectedMatches` 改成"子句 + 3/4 字片段"（4 字 1 个 或 3 字 2 个），prompt 要求写判据词条；详见 `docs/08` |
| 15 | `/login` 缺 `X-Powered-By` 被断言成"nginx 静态文件"，却仍标 `observed` | 两档语义没定义边界，模型把"由缺失推出的结论"当成直接观察 | prompt + `record_fact` 描述钉死：缺失/差异/时序/排除法推出的一律 `inferred` 并写推理链；详见 `docs/08` |

> 共性：**上下文生命周期**（1/3/6）与**状态机竞态**（2/7/8/9）。只看代码看不出来，必须让测试真的跑起来。

---

## 12. 已知限制 / 未实现（按里程碑）

### M3.5

- ~~**goals 拆解 agent**~~ ✅：第 0 轮 `set_goals/set_constraints/add_scope` + `run_tasks.goals_at` 已落地
  （`engine.ensureGoals`、`agent/goals.go`、`store/goals.go`）。
- ~~**差分态势（P1-2）**~~ ✅：`nodes.rev`/`edges.rev` + `store.DeltaSince` + planner 的增量输入与 `refresh_overview`
  已落地。**代价**：新增写路径必须打 `rev`（§0.6）。
- **计划修订**：`revision+1` 没有接口，`off_plan` 只被记录，没有汇总成"计划该改"的信号。
- **步骤预算**：`payload.budget` 预留，worker 还没按步骤预算夹逼。
- **增量的事件压缩**：同一节点一轮内被改多次只出现一次（有意），但"改了几次"没渲染出来。
- **跨进程的版本一致性压力测试**：现在靠"重启必全量"规避崩溃窗口，没有专门的 fuzz。

### M4

- ~~settlement 收尾轮~~ ✅ / ~~compaction、micro-compact~~ ✅ / ~~`steer_work`~~ ✅（M4a，见 `docs/04`）；
- ~~删除流水线~~ ✅ / ~~准入队列~~ ✅ / ~~完整启动恢复~~ ✅（M4b，见 `docs/05`）；
- ~~**流式 + committed 边界**~~ ✅（M4c，见 `docs/06`）。**M4 全部完成。**

### M4c 的已知限制

- 增量 activity 行没拆（一轮仍只写一条合并后的 text/thinking；M5 做逐字 SSE 时再拆）；
- 只有 `chat/completions` 一种 wire 的流式（anthropic / responses 未做）；
- 按角色的 profile 切换（P1-4）没做，所以"已提交后不能换人"目前只是 provider 内部不重试。

### M4b 的已知限制

- 删除是**单进程语义**（内存 `deleting` map）：多实例需要 DB 租约；
- 队列只有 FIFO，没有优先级插队；
- 没有删除进度端点（只有日志 + 一条 system activity）；
- 只清 `data/runs/<id>`，没有别的外部资源（流量库 M5/M6 才进来）。

### M4a 的已知限制

- planner 侧没有压缩（它每轮最多 6 回合，态势每轮重取，收益低）；
- token 估计是启发式（CJK 1 字 1 token），没有接 tokenizer；
- 没有"快到上限时提前提醒模型收拢"——只有撞到才收尾。

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

### 13.1b 新增/修改一条写路径（M3.5 起）

1. 在同一个事务里先写业务行，再做 `rev, err := bumpVersion(ctx, tx, runID)`；
2. `stampNodes(...)` / `stampEdge(...)` 把这次写入的行打上 `rev`；
3. 失败的 CAS 不要提前 bump（先 UPDATE 再判 `RowsAffected`）；
4. 跑 `store.TestDeltaReconstructsFullGraph`：它会告诉你差分有没有丢。

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
16. **第 0 轮拆解必须排在 goalless 闸门之前**：否则新任务一启动就被判"目标都已达成"（seed 目标还没被拆解替换）。
17. **`rev` 是版本号不是计数器**：它会被反复覆盖（同一行每次写入拿新号），不要用它排序或计数；
    也不要在失败的 CAS 之前 `bumpVersion`（浪费版本号且会在增量里留下"没发生的变化"）。
18. **`DeltaSince` 的 `to` 必须先快照**：先用 `rev <= to` 过滤再查，否则"读到一半又有写入"会让窗口与数据不一致。
19. **拆解失败不要写 `goals_at`**：写了就再也不会重试，用户看到的是"系统只把原话当目标"。
20. **收尾轮不能复用 execCtx**：撞 `MaxDuration` 时它已经过期，收尾轮会用独立预算（`Settlement.Timeout`）。
21. **压缩按"轮"切，不按"条"切**：一轮 = assistant + 它后面的 tool 结果；按条切会留下孤立的 tool 消息，服务端直接 400。
22. **`steer_work` 必须回一条 `RoleTool`**（否则 tool_calls 没有结果 → 400）；意图结束时要清空 `steerBox`。
23. **`record_fact` 的入参是 `facts[] + intent_id`**，不是顶层 `summary`/`evidence` —— 写测试时最容易踩。
24. **准入计数只认 `status='running'`**：把 `created` 也算进"占坑"，同时建的任务会互相挤进队列（全部排队、谁都不跑）。
25. **删除要先 rename 文件再删库**：反过来一旦 DB 失败就永久丢数据；DB 成功而文件清理失败**不能回退**状态。
26. **`stop` 就是删除**：钦原没有"停机保号"状态；想保留数据必须用 `pause`（API 文案已写清）。
27. **流式的 `arguments` 是分片**：整块覆盖会拼出半个 JSON，工具层只会报"参数解析失败"。
28. **committed 之后绝不重试**：这是流式唯一的正确性约束；provider 自己负责，别在上层再"补一次"。
29. **`stream_options.include_usage` 必须显式要**：否则流式响应的 token 账全是 0。
30. **"范围"和"入口"是两件事**：范围回答"允许打哪"，`seeds` 回答"从哪开始"。只存范围，
    worker 就会去猜端口（实测 49% 的请求打错端口）。新加的目标类型也要想清楚这两个问题。
31. **任何"由 A 迁移到 B"的状态都必须有回程**：步骤进 `running` 靠 `LinkStepIntent`，
    出去必须有人在意图终态时回收，否则计划图会永远显示"在跑"。
32. **规则匹配别对"整句任务口吻"做全串匹配**：动词与虚词会稀释覆盖率；要切子句、去包装词、
    只匹配具体名词片段。但片段匹配**偏松** —— 同一个事实可能同时满足多个步骤，所以判据词条
    必须要求"有区分度"，别写「登录」「表单」这种单位词。
33. **`observed` 只允许表示"直接看到"**：靠缺失、差异、时序、排除法推出来的结论一律 `inferred`。
    证据门槛挡不住"证据不支持结论"，这一层只能靠 prompt 语义约束。
34. **意图预算只能收紧、不能放大**：`resolveBudget` 取 `min(任务配置, 意图预算)`，配置是天花板；
    越界值夹边界而不是报错 —— 预算是优化项，不该让一次执行直接失败。
35. **http 响应体默认只回 4KB + 落盘指针**：200KB 的 HTML 按 `MaxOutput` 全额回会吃掉
    1/5 个窗口。要全文显式传 `full=true`，别把它做成默认。
36. **看成本看"有效输入"（input − cache_read），不要看名义 input**：`run #24` 名义 107k、
    有效只有 18.6k。按名义值做优化决策会去砍几乎免费的东西。
37. **planner 空转轮先别退避**：两轮 0 意图名义成本 31k、有效只有 ~4k，而其中一轮顺带
    `prove_goal` 收了工 —— 退避会让任务晚一个心跳（≥600s）结束。理由见 `docs/10`。
38. **中间件包 `ResponseWriter` 会让 SSE 静默坏掉**：`statusRecorder` 不实现 `Flush()`，
    `w.(http.Flusher)` 断言失败 → 流接口 500，页面只显示"重连中"。包装时必带
    `Flush()` 与 `Unwrap()`。
39. **推给 SSE 的是内存对象**：`created_at` 由 DB 填，不补就是零值 —— 前端会显示公元 1 年
    （上海 LMT 恰好是 `08:05:43`，很容易被当成时区 bug）。`emit` 成功后补 `CreatedAt`。
40. **控制台静态资源迭代期一律 `Cache-Control: no-cache`**：`max-age=300` 会让"改了 CSS
    刷新没变"，很容易误判成样式规则没生效（本轮就这么被坑过一次）。
41. **IP 目标的覆盖度按 `run_scope.value` 匹配，不是 `net`**：`runplan` 给 IP 字面量只写
    `value`，`net` 为空；只按网段匹配会让这类任务的覆盖率永远是 0/0。
42. **`<span>` 当进度条不生效**：`display:inline` 的宽高被忽略，条形图会全是空的。
    进度条/柱状图的 track 与 fill 都要显式 `display:block`。
43. **`health.llm_configured` 别再硬编码**：曾经写死 `false`，设置页永远显示"模型未配置"。
    现在按 provider 类型判断（`*llm.FakeFactory` = 未配置）。
44. **审批规则要匹配"实际会执行什么"**：模型常省略 `method`（工具默认 GET），
    规则若只看原始入参，"GET 需要审批"永远不命中。`normalizeToolInput` 先补默认值再匹配。
45. **`CreateInterceptRule` 的 `Enabled` 默认是 false**：写测试/脚本时忘记显式置 true，
    规则会被静默跳过（`WHERE enabled` 过滤）。加规则时一律显式给。
46. **`intercept_pending` 没有启动清扫**：崩溃/重启后残留 `pending` 行永远是 pending，
    UI 上还能点批准但已经没人在等（与 ARTEX 同一缺口）。恢复时需要一次性 `timeout`。
47. **`url_template` 归一化只能动 path/query**：把 host 也数字替换掉，
    `http://127.0.0.1:8080/` 会变成 `http://{n}.{n}.{n}.{n}:{n}/`，按端口检索永远搜不到。
48. **`pg_trgm` 装在 public，测试 schema 的 search_path 里没有它**：GIN 索引要写
    `public.gin_trgm_ops`，且索引创建必须包在 EXCEPTION 里，否则一个库没有扩展就让迁移全挂。
49. **失败的请求也要录**：`status=0 + error` 是"当时打不通"的可复核证据，
    只记成功的流量会让报告里的失败结论无据可查。
50. **检索只覆盖 ≤8KB 预览**：更大正文落内容寻址文件，要原文得用 `traffic_get` 按 offset 读文件。
51. **`AddRetest` 只回 id，不回 `created_at`**：接口直接回内存对象时时间戳是 0
    （页面显示 1970）。落库后要补 `CreatedAt`，或统一从 DB 重读。
52. **中文引号别写进 Go 字符串**：报告模板里 `"重放当时的目标状态"` 会让整个文件语法错误
    （`missing ',' in argument list`）。用「」或转义。
53. **psql 命令行传中文会炸**（client_encoding）：seed SQL 写成 UTF-8 文件 + `psql -f`，
    并设 `PGCLIENTENCODING=UTF8`。
54. **新建策略默认 `enabled=false` 会被静默跳过**：`CreateApprovalPolicy` 内部强制置 true
    （撤销走 DELETE），否则"策略写了不生效"（与坑 #45 同一类）。
55. **html/template 的 `Funcs` 必须在 `Parse` 之前**：模板里用了 `dict` 而解析时未注册，
    运行期直接 panic（编译期看不出来）。
56. **连接器失败不能写"已投递"**：台账里 `status='failed'` 的行不能挡住重试，否则一次网络抖动
    会让该事件永远不再推（`connectors.Fanout.Handle` 里判 status）。
57. **中文引号别写进 Go 字符串**（坑 #52 的复现点：报告模板）：用「」或转义。
58. **SSE 广播别走 outbox**：广播要低延迟且可丢；outbox 只处理"必须恰好一次"的副作用。

---

## 15. 当前交付物与 git 状态

```
<待提交> M6c：finding 复测 + Markdown 报告（docs/14）
ad9ba16 M6b：流量录制（工具层录制 + PG 索引）
a5662fd M6a：审批门（规则 → 人工挂起）
138aa6d M5b：链路图 + 覆盖图（自写 SVG 布局）
3525172 控制台重做：按 ARTEX 的仪表盘信息架构
5963694 M5a：activity SSE + 控制台（手写 SPA + go:embed）
30fcc50 P4 观测：每节点用量 + /api/runs/{id}/tokens（docs/10）
f51beba P4：意图级预算 + http 两档返回（docs/09）
566e2c6 修复：种子 URL（runs.seeds）+ 意图终态回收步骤 + 判据匹配重写 + confidence 边界（docs/07、08）
29e5f25 演示：M4 现场效果（假 OpenAI 流式网关 + 收尾轮/队列/删除/流式 TTFT）
be7a1b5 M4c: 流式输出（SSE）+ committed 边界（未提交才可重试）+ TTFT 观测
5c33ad1 M4b: 删除流水线（屏障→排水→两阶段）+ 并发准入队列（FIFO）+ 完整启动恢复
4c1be78 M4a: settlement 收尾轮 + compaction/micro-compact + steer_work 实时纠偏
1ea3168 M3.5: 第 0 轮目标拆解（set_goals/set_constraints/add_scope）+ 差分态势（rev 版本戳/增量输入/refresh_overview）
d73d6f4 文档：新增 HANDOFF.md（环境/状态机/已修缺陷/扩展指南），README 加入口
077f3d3 工具链：接入 mingw-w64，scripts/race.ps1 跑 -race；全量竞态检测通过
00250f4 工具链：scripts/go.ps1 改为优先系统 64 位 Go（amd64），便携版兜底
e3a7cde M3a: 计划图（step 依赖 + expected_evidence 规则判定）+ 覆盖度 + off_plan 逃生门；切换 64 位 Go
bb6b6de M2: planner 主链（事件唤醒/心跳/六闸门/add_intent/prove_goal）+ OpenAI 兼容 provider + 超时收尾 + 启动恢复
bcb933a 修复暂停路径：settle 用不可取消的 ctx 写库；Work 支持被 Pause 取消在跑的执行
394f5b4 M0/M1: 钦原骨架——幂等迁移、CAS 队列、工具层、worker 闭环、CLI/API 与验收测试
```

P0–P4、M5a、M5b、M6 已提交（`566e2c6`…`62e5a5f`）。
**M7（平台化七件套）已在工作区完成并提交**：schema 增 activity span 列 / outbox /
approval_policies+grants / campaigns+campaign_scope / connector_deliveries；
新包 `internal/{trace,outbox,connectors,report,graphstore/graphstore,graphstore/memstore}`；
新 API 见 `docs/15 §9`。工作区应为干净（`git status` 无改动）。

`docs/00–15` 是设计与验收记录；本文是状态快照。

**验证一切正常的三条命令**：

```powershell
pwsh -File scripts/go.ps1 test ./... -count=1     # 146 个测试全绿
pwsh -File scripts/race.ps1 ./...                 # 零数据竞争
pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo  # ✓ demo 通过
pwsh -File scripts/demo/demo-m4.ps1               # M4 现场演示（收尾轮/队列/删除/流式 TTFT）
```

---

## 16. 需要人拍板的问题（接手 AI 不要自己决定）

1. ~~M3.5 还是 M4~~ → 已选 M3.5 并完成；**下一个是 M4 还是 M5**（长跑可靠性 vs 可观测/前端）。
2. **真实模型 live 冒烟**：需要 `base_url + model`（key 走环境变量）；这一步能暴露 prompt 质量与工具调用质量，是 fake provider 测不出来的。
3. **`expectedMatches` 的匹配规则**（中文 2-gram + 60% 覆盖）是否够准；太松会假完成，太紧会一直等人。
4. **`add_intent` 强制 `step_id`** 是否会让 planner 为了过校验硬凑步骤。
5. **许可证**：当前 MIT；如果将来要复用 ARTEX/norma 的任何代码，必须先停下来讨论 AGPL 传染问题。
6. **生产形态**：PG 是继续便携实例还是独立实例；是否需要鉴权/多用户；部署目标（单二进制 + 反代）。
