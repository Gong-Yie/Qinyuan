# 钦原（Qinyuan）

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)

授权 Web 安全测试 Agent —— Go 后端 + PostgreSQL，把探索过程外化成一张**可查询、可审计、可重启**的图，让 LLM 在图上反复"规划 → 执行 → 写回"。

> **当前进度：M0 + M1 + M2 + M3a + M3.5 + M4a + M4b + M4c + M5 已跑通。** 库与迁移、CAS 队列、实体去重、工具层、worker 执行循环、九分支收尾、planner（事件唤醒 / 心跳 / add_intent / prove_goal）、OpenAI 兼容 provider、超时收尾、启动恢复、**计划图（step 依赖 + 规则化 done）**、**覆盖度**、**第 0 轮目标拆解（set_goals / set_constraints / add_scope）**、**差分态势（rev 版本戳 + 增量输入 + refresh_overview）**、**收尾轮（撞预算不白跑）**、**上下文压缩**、**`steer_work` 实时纠偏**、最小 REST、CLI、端到端 demo、自动化验收都在。
> M4b 再补上：**删除流水线**（屏障→排水→文件暂存→DB 级联→提交后真删）、**并发准入队列**（持久化 FIFO）、**完整启动恢复**（含暂存删除收尾）。
> M4c 再补上：**流式输出**（SSE，`tool_calls` 按 index 合并）+ **committed 边界**（未提交才可重试，已提交绝不重放）+ TTFT 观测。
> M5 再补上：**控制台**（`go:embed` 手写 SPA：目标/计划/意图｜活动带｜成本/覆盖/事实）与 **activity SSE**（任务级一条流、`Last-Event-ID` 从 DB 补齐、断线自动重连）。
> M6 已全部完成；M7–M9 补上平台化能力（trace/outbox/战役/报告/连接器）、控制台新页面与**鉴权**。
> M13/M14 由一次真实 CTF 复盘驱动：修掉空流/计划图假完成等五处缺陷，并补上**受控归档工具**
> （`archive_list` / `archive_read` / `strings`，只读、不落盘）；M15 补上**停摆重规划**
> （0 意图且没有在跑意图时立即重规划，不再干等 600s 心跳）；M16 在 real run 里抓到
> "推理吃光 `max_tokens`"，改成**自动翻倍预算再试一次**。
> 还没有：多用户/SSO、批量复测的任务化计划（CI 已能单条回写）。
> 设计与取舍见 [`docs/00-设计定稿.md`](docs/00-设计定稿.md)、[`01-M2-规划者与模型接入.md`](docs/01-M2-规划者与模型接入.md)、[`02-M3-计划图与覆盖度.md`](docs/02-M3-计划图与覆盖度.md)、[`03-M3.5-目标拆解与差分态势.md`](docs/03-M3.5-目标拆解与差分态势.md)、[`04-M4a-撞预算不白跑.md`](docs/04-M4a-撞预算不白跑.md)、[`05-M4b-删除与准入.md`](docs/05-M4b-删除与准入.md)、[`06-M4c-流式与committed.md`](docs/06-M4c-流式与committed.md)、[`11-M5-控制台.md`](docs/11-M5-控制台.md)、[`15-M7-平台化七件套.md`](docs/15-M7-平台化七件套.md)、[`16-M8-控制台与DTO迁移.md`](docs/16-M8-控制台与DTO迁移.md)。
> 设计与取舍见 [`docs/00-设计定稿.md`](docs/00-设计定稿.md)、[`01-M2-规划者与模型接入.md`](docs/01-M2-规划者与模型接入.md)、[`02-M3-计划图与覆盖度.md`](docs/02-M3-计划图与覆盖度.md)、[`03-M3.5-目标拆解与差分态势.md`](docs/03-M3.5-目标拆解与差分态势.md)、[`04-M4a-撞预算不白跑.md`](docs/04-M4a-撞预算不白跑.md)、[`05-M4b-删除与准入.md`](docs/05-M4b-删除与准入.md)、[`06-M4c-流式与committed.md`](docs/06-M4c-流式与committed.md)、[`11-M5-控制台.md`](docs/11-M5-控制台.md)、[`15-M7-平台化七件套.md`](docs/15-M7-平台化七件套.md)、[`16-M8-控制台与DTO迁移.md`](docs/16-M8-控制台与DTO迁移.md)、[`17-M9-鉴权.md`](docs/17-M9-鉴权.md)、[`18-M12-附件.md`](docs/18-M12-附件.md)、[`19-CTF1-复盘.md`](docs/19-CTF1-复盘.md)、[`20-M14-归档与字符串工具.md`](docs/20-M14-归档与字符串工具.md)、[`21-M15-停摆重规划.md`](docs/21-M15-停摆重规划.md)、[`22-M16-推理预算升级.md`](docs/22-M16-推理预算升级.md)、[`23-CTF1-retest-实跑.md`](docs/23-CTF1-retest-实跑.md)、[`24-执行沙箱设计.md`](docs/24-执行沙箱设计.md)。
> **新接手（人或 AI）请先读 [`HANDOFF.md`](HANDOFF.md)**：环境、状态机、已修缺陷、扩展指南都在那里。

---

## 为什么是"图 + 调度器"，而不是一个聊天机器人

```
                 ┌──────────── 事件驱动 ────────────┐
                 ▼                                  │
  planner（M2）── add_intent ──► frontier（CAS 队列）──► worker ──┐
                 ▲                                  │            │
                 └────── notify ─────────────────── │  record_fact / report_finding / insert_assets
                                                    ▼            │
                              nodes / edges / anchors（DB 是唯一真相源）
```

- **上下文会丢、进程会挂** → 计划与状态必须落库，而不是留在对话历史里；
- **LLM 判断不可靠** → "什么算完成、什么算重复、什么算越界"由数据库约束和代码判定；
- **单次能力有限** → 用多个执行单元 + 一条共享链路把它变成可累积的过程。

---

## 快速开始

### 0. 前置

- Go 1.26+（**64 位**；本机已装在 `C:\Program Files\Go`，`scripts/go.ps1` 会优先用它，找不到才退回项目便携版）
- PostgreSQL（本仓库自带便携脚本，不装服务、不需要管理员）

### 1. 起数据库

```powershell
# 首次：解包 + initdb + 建库（需要先有 .devdb/cache/pg17.zip）
pwsh -File scripts/devdb.ps1 init

# 日常：在被作业对象管理的环境里（CI / agent 工具）用前台模式更可靠
pwsh -File scripts/devdb.ps1 serve
```

默认：`127.0.0.1:55432`，用户 `postgres`，trust 认证，库 `qinyuan` / `qinyuan_test`。
运行时与数据目录在 `%LOCALAPPDATA%\qinyuan`（**ASCII 路径**）—— PostgreSQL 的 `initdb` 在 Windows 上无法处理含中文的数据目录。

### 2. 跑起来

```powershell
go run ./cmd/qinyuan migrate        # 幂等迁移，可反复执行
go run ./cmd/qinyuan demo           # 端到端演示：假目标 + 假模型，不联网
go run ./cmd/qinyuan serve          # HTTP API：127.0.0.1:8787
```

只想本机用：`serve` 默认只监听 `127.0.0.1`，不需要令牌。
**要监听 `0.0.0.0`（或任何非回环地址）必须先配令牌，否则会拒绝启动**：

```powershell
$env:QINYUAN_AUTH_TOKEN = "<32+ 字符随机串>"     # 单令牌（admin）
# 或分角色：$env:QINYUAN_AUTH_TOKENS = "读令牌:viewer,操作令牌:operator,管理令牌:admin"
go run ./cmd/qinyuan serve --addr 0.0.0.0:8787
```

详见 [`docs/17-M9-鉴权.md`](docs/17-M9-鉴权.md)。

`demo` 会自检并打印：

```
✓ demo 通过：范围闸门 → HTTP 探测 → 实体/事实写回 → 意图 done → 配对完整
```

### 3. 接真实模型（M2）

```powershell
# .env.local（不要提交）
QINYUAN_LLM_PROVIDER=openai
QINYUAN_LLM_BASE_URL=https://api.deepseek.com/v1   # 或 https://api.openai.com/v1
QINYUAN_LLM_API_KEY=sk-...
QINYUAN_LLM_MODEL=deepseek-chat
```

任何 OpenAI Chat Completions 兼容后端都行（DeepSeek / OpenAI / 自建网关 / vLLM）。
启动日志会打印 `provider=openai model=... key=***xxxx`，不会打印完整 key。

```powershell
go run ./cmd/qinyuan run create --goal "确认示例站点的服务指纹与登录入口" --target https://example.com/
go run ./cmd/qinyuan work --run 1        # planner + worker 一起跑，到终态退出
```

### 4. 手工走一遍

```powershell
go run ./cmd/qinyuan run create --goal "确认示例站点的服务指纹" --target http://127.0.0.1:8080/
go run ./cmd/qinyuan intent add --run 1 --summary "请求首页并记录 Server / 标题"
go run ./cmd/qinyuan work --run 1 --once
go run ./cmd/qinyuan status --run 1
```

### 5. 测试
```powershell
$env:QINYUAN_TEST_DSN="postgres://postgres@127.0.0.1:55432/qinyuan_test?sslmode=disable"
go test ./...
go test ./internal/store/ -run TestClaimConcurrentNoDuplicates -count=20   # 并发用例反复跑
```

> 竞态检测：`pwsh -File scripts/race.ps1`（64 位 Go + mingw-w64；当前全量 `-race` 已通过，无数据竞争）。

### 6. 看 M4 的实际效果（不用真实模型）

```powershell
pwsh -File scripts/demo/demo-m4.ps1
```

它起一个本地"假 OpenAI 流式网关"（`scripts/demo/fakegw.go`），让 `serve` 走**真实适配器**，
然后现场演示：MaxTurns=1 → 主循环写一条事实 → **收尾轮**再写一条；并发上限=1 时第二个任务
**排队**；删除第一个任务后第二个**自动补位**；每轮都能看到 **TTFT（首字延迟）**。
演示用独立 schema（`m4demo`），跑完自动删除，不碰开发库。

---

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 版本 / 库可用性 |
| GET | `/api/runs` | 任务列表（派生状态） |
| POST | `/api/runs` | 建任务 `{goal, description, target, timeout_seconds, worker_count}` |
| GET | `/api/runs/{id}` | 详情（含意图、目标） |
| POST | `/api/runs/{id}/control` | `{action: start\|pause\|resume}`（stop 尚未实现） |
| GET | `/api/runs/{id}/intents` | 意图列表 |
| POST | `/api/runs/{id}/intents` | 手投一条意图 |
| GET | `/api/runs/{id}/nodes?kind=` | 按类型列节点 |
| GET | `/api/runs/{id}/activity?limit=` | 过程记录 |
| GET | `/api/runs/{id}/overview` | 态势（含计划图 ready 步骤；planner 预取同一份） |
| GET | `/api/runs/{id}/events?since=&limit=` | 差分态势：`(since, 当前版本]` 内的节点/边变化 |
| GET | `/api/runs/{id}/coverage` | 覆盖度（只认 fact 锚点，排除 origin） |
| POST | `/api/runs/{id}/work` | 手动触发一次领取+执行 |
| GET | `/api/runs/{id}/timeline` | 执行时间线（span 树；`?stats=1` 给 p50/p95/p99） |
| GET | `/api/outbox` · `/api/outbox/stats` | outbox 积压/死信（`POST /api/outbox/{id}/replay` 重放） |
| GET/POST | `/api/campaigns` · `/api/campaigns/{id}` | 战役：总覆盖度/进度/重复投入 |
| POST | `/api/campaigns/{id}/control` | 战役级暂停/恢复（级联到子 run） |
| GET/POST | `/api/approval-policies` | 审批策略（预授权须带限速+到期） |
| POST | `/api/approvals/batch-decide` | 批量批准/拒绝（冲突项不覆盖） |
| GET | `/api/approval-grants` | 预授权放行审计 |
| POST | `/api/runs/{id}/approval-mode` | 切换任务级审批模式：`manual` / `auto` / `off` |
| GET | `/api/runs/{id}/report?format=md\|html\|zip\|json` | 可复现报告（默认脱敏，`full=1` 需授权） |
| GET | `/api/connectors/deliveries` | 外部集成投递台账 |
| POST | `/api/runs/{id}/sync-tickets` · `/ci-export` · `/ci-result` | 工单回写 / CI 导出 / 回归回写 |
| POST/GET | `/api/runs/{id}/attachments` | 任务附件：上传（multipart，字段 `file`）/ 列表 |
| GET/DELETE | `/api/runs/{id}/attachments/{aid}` | 下载 / 删除附件 |

错误统一 `{"error": "中文可读原因"}`。所有时间戳为 unix 秒。

---

## 目录

```
cmd/qinyuan            CLI 与接线
internal/
  config               配置：ENV > .env.local > 默认，返回来源；路径转绝对
  model                领域类型 + DeriveStatus（全站唯一状态推导）
  store                唯一数据访问层：迁移、CAS、去重、activity
    schema.sql         幂等 DDL（重启即迁移）
  domain               归一化 + 范围闸门（纯函数）
  llm                  provider 抽象 + 可编程假 provider
  agent                prompt 契约、工具面、单次执行循环
  engine               领取、执行、九分支收尾、暂停/恢复/kill
  server               HTTP 入口（只做解析与 DTO）
  runplan              用户输入 → 建 run 计划
  testkit              测试隔离库与夹具
scripts/devdb.ps1      便携 PostgreSQL 管理
docs/00-设计定稿.md    决策、边界、偏离、验收、路线图
docs/03-M3.5-….md      目标拆解与差分态势的设计/验收/坑
```

---

## 安全边界（默认拒绝）

- **范围闸门在写入之前**：`run_scope` 为空 = 什么都不授权；越界实体与 URL 根本进不来。
- **出网守卫**：拨号层拒绝 loopback / 链路本地（含 `169.254.169.254`）/ 组播 / 未指定；**不拒绝 RFC1918**（内网自建服务是合法用法，这是刻意取舍）。
- **重定向**：只允许同主机，≥5 次拒绝（凭据常在 URL 里）。
- **证据门槛**：`fact` 必须有 `evidence`；`finding` 必须有可复现证据，否则只能记 `inferred` 事实。
- **鉴权（M9）**：HTTP 层默认只监听回环；一旦监听非回环**必须有令牌**（否则拒绝启动）。
  令牌分 `viewer/operator/admin` 三角色，控制台走会话 Cookie + CSRF 双提交；改审批策略必须是 admin。
- **审批模式（M21）**：默认 `manual`；`auto` 把命中 `ask` 的调用自动放行（`deny` 仍优先）；
  `off` 跳过整个审批门（含 `deny`），只给受控任务用。全局 `QINYUAN_APPROVAL_MODE`，任务级
  `approval_mode` 可覆盖；控制台新建/详情页可切换。
- `QINYUAN_ALLOW_LOOPBACK=1` 只在 demo/自测用，它会削弱 SSRF 防护。

> 钦原只应在**获得明确授权**的目标上使用。范围闸门是工具，不是法律许可。

---

## 路线图

| 里程碑 | 内容 | 出口标准 |
|---|---|---|
| ~~M2~~ ✅ | planner（事件唤醒 / debounce / 心跳 / `add_intent` / `prove_goal`）+ OpenAI 兼容 provider + 超时收尾 + 启动恢复 | 目标达成自动 done；重启不丢 |
| ~~M3a~~ ✅ | 计划图（`plan/step` + `depends_on` + `expected_evidence`）+ 规则化 ready/done + 覆盖度 + `off_plan` 逃生门 | 5 步链只有 1 步 ready；事实命中自动解锁下一步；覆盖率只认 fact |
| ~~M3.5~~ ✅ | 第 0 轮 goals 拆解（`set_goals`/`set_constraints`/`add_scope`）+ 差分态势（P1-2：`rev` 版本戳 / 增量输入 / `refresh_overview`） | 多目标拆分；满规模图上态势 −64%（实测 36%） |
| ~~M4a~~ ✅ | settlement 收尾轮 + compaction/micro-compact + `steer_work` | 撞 `MaxTurns` 后事实仍写回；长会话不越窗；纠偏在下次工具调用前生效 |
| ~~M4b~~ ✅ | 删除流水线 + 准入队列 + 完整启动恢复 | 删除期间无新写入；队列按 `queued_at` 补位；重启续跑 |
| ~~M4c~~ ✅ | 流式（SSE）+ committed 边界 | 未提交可重试、已提交绝不重放；断流保留部分结果 + TTFT |
| ~~M5a~~ ✅ | activity SSE + 手写 SPA + `go:embed`（`docs/11`） | 断网 30s 回来不重复不缺口；三栏仪表盘实时跟进 |
| ~~M5b~~ ✅ | 链路图（DFS 断环→最长路径分层→重心法）+ 覆盖图（同心圆分层 + 折叠） | 大图不抖、可折叠展开、缩放平移 |
| ~~M6a~~ ✅ | 审批门（规则 → 人工挂起，默认只对 DELETE/PUT/PATCH 要审批） | 危险动作在工具执行前被拦住；批准后继续 |
| ~~M6b~~ ✅ | 流量录制（工具层录制 + PG 索引 + 证据快照） | 请求/响应可检索可回放、能绑到 finding |
| ~~M6c~~ ✅ | finding 复测（重放+断言）+ Markdown 报告 | 三种 finding 分别 verified/failed/unverifiable；报告带可执行 curl |
| ~~M7~~ ✅ | 平台化七件套：trace 时间线 / outbox / 可插拔 GraphStore / 战役视图 / 审批 SLA / 可复现报告 / 双向集成（`docs/15`） | 时间线能还原"轮→意图→工具"；推 5 次只手 1 个单；报告 zip 自包含 |
| ~~M8~~ ✅ | 控制台补齐：战役页 / 运维页 / 时间线 Tab / 报告包·CI·同步工单 / 批量审批 + P2-4 S3 DTO 迁移（`docs/16`） | 浏览器实测六页；`graphstore` 不再依赖 `store`；146 用例 + `-race` 全绿 |
| ~~M9~~ ✅ | 鉴权：Bearer 令牌（viewer/operator/admin）+ 会话 Cookie + CSRF + 登录限速 + 非回环必须有鉴权（`docs/17`） | 无凭据 401；viewer 写 403；CSRF 缺失 403；`0.0.0.0` 无令牌拒绝启动；156 用例全绿 |
| ~~M12~~ ✅ | 任务附件：上传（内容寻址）+ 清单注入 + 只读工具 `list_attachments`/`read_attachment`（`docs/18`） | 附件进态势；agent 只读、无路径参数；逃逸路径被拒 |
| ~~M21~~ ✅ | 审批模式：`manual` / `auto`（ask 自动放行，deny 仍拦）/ `off`（跳过审批门）；全局 + 任务级 | 无人值守不再卡 pending；auto 保留 deny；off 有二次确认 |

---

## 许可

[Apache License 2.0](LICENSE)（含专利授权与商标条款；分发时保留 [`NOTICE`](NOTICE)）。
本项目为 clean-room 实现，不含 AGPL 的 ARTEX / norma 源代码。
