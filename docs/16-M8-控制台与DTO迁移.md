# M8 · 控制台补齐 + P2-4 收尾（DTO 迁移）

> 依据：M7 交付后的两个自然缺口 ——
> ① 新能力（时间线 / 战役 / 审批策略 / 报告包 / CI / 台账）只有 API，没有页面；
> ② P2-4 只到 S1–S2，`graphstore` 还在 import 具体存储（DTO 类型住在 store）。
>
> 交付纪律：页面全部**在真实浏览器里点过**（不是"应该能渲染"），迁移后全量测试与 `-race` 仍绿。

---

## 1. 控制台补了什么

| 位置 | 新增 | 对应 API |
|---|---|---|
| 侧边栏 · 工作区 | **战役**（列表 / 详情） | `GET/POST /api/campaigns`、`GET /api/campaigns/{id}`、`/control`、`/runs`、`/scope` |
| 侧边栏 · 系统 | **运维**（死信 / 投递台账 / 审批策略 / 放行审计） | `/api/outbox*`、`/api/connectors/deliveries`、`/api/approval-policies`、`/api/approval-grants` |
| 任务详情 · Tab | **时间线**（span 树 + p50/p95/p99 + 慢调用过滤） | `GET /api/runs/{id}/timeline?stats=1` |
| 任务详情 · 头部 | 报告 MD / HTML / **报告包**（zip）/ **CI 导出** / **同步工单** | `GET /api/runs/{id}/report?format=`、`POST /ci-export`、`POST /sync-tickets` |
| 审批页 | **全选 + 批量批准/拒绝**（冲突项列出、不覆盖别人决定） | `POST /api/approvals/batch-decide` |

三个交互细节：

1. **时间线点开看属性**：每行点一下在右侧抽屉里显示 span 的 `attrs`（`prompt_bytes`、`situation`、
   `tokens`、`tool` 等），排障时不用再去翻 activity；
2. **战役页的护栏**：预授权策略表单里，选 `allow` 而没填限速/有效期会就地拒绝（与 store 的校验同源）；
3. **死信重放**是一等操作：`重放` 按钮直接调 `POST /api/outbox/{id}/replay`，成功后整页刷新。

浏览器实测（截图在 `docs/screenshots/m8-*.png`）：

- `m8-campaigns.png` / `m8-campaign-detail.png`：战役列表与详情（覆盖度 3% = 1/31、目标 1/2、子 run 表）；
- `m8-timeline.png`：run #35 的 16 个 span，层级 `intent.execute → llm.call/tool.call`、`planner.round → …`；
- `m8-ops.png`：运维页（outbox 0 / 连接器未配置 / 策略 0 的空状态）；
- `m8-approvals-batch.png`：两条待审批 + 批量工具栏。

> 批量操作也做了**真实点击验证**：种两条 pending → 点"全选"→ 点"批量批准"→
> DB 两条都变 `allowed`、`decided_by=ui-batch`。

---

## 2. P2-4 S3：DTO 迁到 model

**改了什么**：把三个视图类型组从 `store` 搬到 `internal/model/views.go`：

| 类型 | 原来 | 现在 |
|---|---|---|
| `Coverage` / `KindCoverage` | `store/coverage.go` | `model/views.go` |
| `PlanGraph` / `StepView` | `store/plans.go` | `model/views.go` |
| `CampaignView` / `CampaignDedupHit` | `store/campaigns.go` | `model/views.go` |

**做法**：store 里保留**类型别名**（`type Coverage = model.Coverage`），所以几百个调用点零改动，
但依赖方向变了：

```
graphstore（契约）──import──> model（领域类型）
      ▲
      │ 实现
store / memstore（各自满足 Graph）
```

- `graphstore/graphstore.go` 不再 import `store`（`go list -deps` 可证）；
- 编译期断言从契约包挪到实现方：`internal/store/graphstore_assert.go` 里
  `var _ graphstore.Graph = (*Store)(nil)`；
- `memstore` 也不再 import store（原本只为借 `ErrNotFound`，现在用自己的错误文案）。

**为什么这算"收尾"而不是"又一层抽象"**：没有新增接口方法、没有改任何 SQL、没有动写路径；
只是把"两个实现共享的数据形状"放到了两者共同的下游。风险接近零，收益是契约包终于不依赖具体存储。

---

## 3. 验证

```powershell
pwsh -File scripts/go.ps1 test ./... -count=1   # 146 个用例全绿（含 store/agent/report/graphstore 迁移回归）
pwsh -File scripts/race.ps1 ./...               # 零数据竞争
pwsh -File scripts/go.ps1 run ./cmd/qinyuan demo
```

浏览器：`serve` 后依次打开 `#/campaigns`、`#/campaigns/1`、`#/ops`、`#/runs/35/timeline`、
`#/approvals`，逐个截图并核对数字（见 §1）。

---

## 4. 已知限制 / 下一步

1. **控制台没有自动化测试**：断言仍是"人看截图"，没有 Playwright 之类的回归；
2. **预授权策略页没有编辑**：只能新增/撤销（改 = 撤销再建），这是有意的简化；
3. **时间线没有"按意图过滤"**：`?name=` 与 `min_ms` 已有，按 `node_id` 过滤要等前端加参数；
4. **CI 导出只写了脚本**，没有"把回归结果批量提交"的入口（单条有 `ci-result`）；
5. **`agent` 仍 import `store`**（worker/planner 的写路径与事务强耦合）。要彻底解耦，下一步是把
   `AddIntent/AddFact/AddFinding` 的入参类型也搬进 model —— 那是 P2-4 S4，属于 P2-1 拆包的一部分。
