-- 钦原 · schema（幂等，每次启动全量执行）
-- 依据：实现规格/01-数据模型.md
-- 约定：所有语句必须可重复执行；约束升级用 DO $$ 检测后重建。
-- 偏离项（有意为之，见 docs/00-设计定稿.md）：
--   1) findings 是视图，不是双写表（nodes 是 finding 的唯一真相源）；
--   2) entities.auth/params/open_ports 用 JSONB 数组（等价于 JSONB[]，合并语句更简单）。

-- ---------------------------------------------------------------------------
-- 0. 工具函数
-- ---------------------------------------------------------------------------

-- 非法 inet 变 NULL 而不是抛 22P02（脏数据防护）
CREATE OR REPLACE FUNCTION try_inet(t TEXT) RETURNS inet
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    RETURN t::inet;
EXCEPTION WHEN others THEN
    RETURN NULL;
END $$;

-- ---------------------------------------------------------------------------
-- 1. settings：一次性迁移旗标、运行时偏好
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 2. 多租户归属（M1 只建表，归属重算在 M4/P3 接）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS orgs (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS org_scope (
    id     BIGSERIAL PRIMARY KEY,
    org_id BIGINT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    kind   TEXT NOT NULL CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    domain TEXT,
    net    CIDR,
    value  TEXT,
    CHECK ((kind = 'domain' AND domain IS NOT NULL) OR
           (kind IN ('ip','cidr') AND net IS NOT NULL) OR
           (kind IN ('icp','keyword') AND value IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_scope_domain ON org_scope(org_id, domain) WHERE kind = 'domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_scope_net    ON org_scope(org_id, net)    WHERE kind IN ('ip','cidr');
CREATE UNIQUE INDEX IF NOT EXISTS uq_scope_value  ON org_scope(org_id, kind, value) WHERE kind IN ('icp','keyword');

-- ---------------------------------------------------------------------------
-- 3. runs：一次探索
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS runs (
    id          BIGSERIAL PRIMARY KEY,
    goal        TEXT NOT NULL,
    description TEXT,
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','achieved','failed')),
    round_no    BIGINT NOT NULL DEFAULT 0,
    version     BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- run 的业务状态（与 runs.status 的“知识层是否达成”分开）
CREATE TABLE IF NOT EXISTS run_tasks (
    run_id            BIGINT PRIMARY KEY REFERENCES runs(id) ON DELETE RESTRICT,
    name              TEXT,
    status            TEXT NOT NULL DEFAULT 'created'
                      CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused            BOOLEAN NOT NULL DEFAULT false,
    queued            BOOLEAN NOT NULL DEFAULT false,
    queued_at         BIGINT,
    timeout_seconds   INT NOT NULL DEFAULT 0,
    first_run_at      BIGINT NOT NULL DEFAULT 0,
    deadline_at       BIGINT NOT NULL DEFAULT 0,
    heartbeat_seconds INT NOT NULL DEFAULT 600,
    worker_count      INT NOT NULL DEFAULT 2,
    completed_at      BIGINT NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_run_tasks_status ON run_tasks(status, queued_at);

-- ---------------------------------------------------------------------------
-- 4. nodes：图的核心
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS nodes (
    id               BIGSERIAL PRIMARY KEY,
    run_id           BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'open',
    payload          JSONB NOT NULL DEFAULT '{}',
    priority         INT NOT NULL DEFAULT 5,
    owner            TEXT,
    origin           TEXT,
    blocked_reason   TEXT,
    delete_reason    TEXT,
    content_version  INT NOT NULL DEFAULT 0,
    cold_since_round BIGINT,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_node_kind CHECK (kind IN ('goal','intent','fact','finding','hint','digest')),
    CONSTRAINT ck_node_state CHECK (
        (kind = 'intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind = 'goal'    AND state IN ('open','met','abandoned')) OR
        (kind = 'fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind = 'finding' AND state IN ('confirmed','dismissed')) OR
        (kind = 'hint'    AND state IN ('active','consumed')) OR
        (kind = 'digest'  AND state IN ('active','superseded'))
    )
);

CREATE INDEX IF NOT EXISTS idx_frontier    ON nodes(run_id, priority DESC) WHERE kind = 'intent' AND state = 'open';
CREATE INDEX IF NOT EXISTS idx_nodes_kind  ON nodes(run_id, kind, id DESC);
CREATE INDEX IF NOT EXISTS idx_nodes_state ON nodes(run_id, kind, state);

-- ---------------------------------------------------------------------------
-- 5. edges
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS edges (
    run_id     BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    src_id     BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    rel        TEXT NOT NULL,
    dst_id     BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers'))
);
CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_id, rel);

-- ---------------------------------------------------------------------------
-- 6. entities：领域实体（跨 run 共享，去重交给部分唯一索引）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS entities (
    id             BIGSERIAL PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('root_domain','ip','subdomain','service','endpoint','app')),
    owner_org_id   BIGINT REFERENCES orgs(id) ON DELETE SET NULL,
    owner_source   TEXT NOT NULL DEFAULT 'explicit' CHECK (owner_source IN ('explicit','scope')),
    run_ids        BIGINT[] NOT NULL DEFAULT '{}',
    domain         TEXT,
    root_domain    TEXT,
    record_type    TEXT,
    ip             TEXT,
    segment        CIDR,
    port           INT CHECK (port BETWEEN 1 AND 65535),
    url            TEXT,
    method         TEXT,
    service_type   TEXT CHECK (service_type IN ('http','other')),
    service_name   TEXT,
    status_code    INT,
    content_length BIGINT,
    page_title     TEXT,
    technologies   TEXT[] NOT NULL DEFAULT '{}',
    open_ports     JSONB NOT NULL DEFAULT '[]',
    auth           JSONB NOT NULL DEFAULT '[]',
    params         JSONB NOT NULL DEFAULT '[]',
    extra          JSONB NOT NULL DEFAULT '{}',
    first_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_root_domain ON entities(domain) WHERE kind = 'root_domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_ip          ON entities(ip)     WHERE kind = 'ip';
CREATE UNIQUE INDEX IF NOT EXISTS uq_subdomain   ON entities(domain, COALESCE(record_type, '')) WHERE kind = 'subdomain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_service_http  ON entities(url) WHERE kind = 'service' AND service_type = 'http';
CREATE UNIQUE INDEX IF NOT EXISTS uq_service_other ON entities(COALESCE(domain, ''), COALESCE(ip, ''), port, service_name)
    WHERE kind = 'service' AND service_type = 'other';
CREATE UNIQUE INDEX IF NOT EXISTS uq_endpoint ON entities(url, method) WHERE kind = 'endpoint';

CREATE INDEX IF NOT EXISTS idx_ent_run  ON entities USING GIN(run_ids);
CREATE INDEX IF NOT EXISTS idx_ent_dom  ON entities(domain) WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ent_seg  ON entities USING GIST(segment inet_ops) WHERE segment IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ent_tech ON entities USING GIN(technologies) WHERE kind = 'service';

-- ---------------------------------------------------------------------------
-- 7. anchors：两图的接缝
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS anchors (
    node_id   BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    entity_id BIGINT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, entity_id)
);
CREATE INDEX IF NOT EXISTS idx_anchor_entity ON anchors(entity_id);

-- ---------------------------------------------------------------------------
-- 8. activity：过程库（只追加）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS activity (
    id                 BIGSERIAL PRIMARY KEY,
    run_id             BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    node_id            BIGINT REFERENCES nodes(id) ON DELETE SET NULL,
    worker             TEXT,
    kind               TEXT NOT NULL,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    metadata           JSONB NOT NULL DEFAULT '{}',
    input_tokens       INT,
    output_tokens      INT,
    cache_read_tokens  INT,
    cache_write_tokens INT,
    segment            INT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_act_node     ON activity(run_id, node_id, id);
CREATE INDEX IF NOT EXISTS idx_act_since    ON activity(run_id, id);
CREATE INDEX IF NOT EXISTS idx_act_toolcall ON activity(run_id, tool_use_id, id) WHERE kind IN ('tool_use','tool_result');
CREATE INDEX IF NOT EXISTS idx_act_worker   ON activity(run_id, worker, id);
CREATE INDEX IF NOT EXISTS idx_act_usage    ON activity(run_id)
    INCLUDE (input_tokens, output_tokens, cache_read_tokens, cache_write_tokens) WHERE kind = 'result';

-- ---------------------------------------------------------------------------
-- 9. constraints：操作约束（allow/deny，注入 prompt）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS constraints (
    id         BIGSERIAL PRIMARY KEY,
    run_id     BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('allow','deny')),
    text       TEXT NOT NULL,
    origin     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_constraints_run ON constraints(run_id, kind);

-- ---------------------------------------------------------------------------
-- 10. run_scope：授权边界 + 覆盖度分母
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS run_scope (
    id         BIGSERIAL PRIMARY KEY,
    run_id     BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('org','root_domain','subdomain','ip','cidr','icp','keyword')),
    domain     TEXT,
    net        CIDR,
    org_id     BIGINT,
    value      TEXT,
    source     TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('auto','agent','manual')),
    reason     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_run_scope ON run_scope(run_id, kind,
    COALESCE(domain, ''), COALESCE(net::text, ''), COALESCE(org_id, 0), COALESCE(value, ''));

-- ---------------------------------------------------------------------------
-- 11. findings：视图（不是表）
--     nodes(kind='finding') 是唯一真相源，避免 ARTEX 的双写不一致
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW findings AS
SELECT n.id                                       AS id,
       n.run_id                                   AS run_id,
       n.id                                       AS node_id,
       COALESCE(n.payload->>'name', n.payload->>'summary') AS name,
       n.payload->>'severity'                     AS severity,
       n.payload->>'vulnclass'                    AS vuln_class,
       n.payload->>'summary'                      AS summary,
       n.payload->>'evidence'                     AS evidence,
       n.state                                    AS status,
       n.created_at                               AS created_at
FROM nodes n
WHERE n.kind = 'finding';

-- ---------------------------------------------------------------------------
-- 12. 数据修复（幂等，每次启动执行）
--     M1 建的历史 run 没有 goal 节点；M2 起每个 run 必须至少有一个目标，
--     否则 planner 的"目标达成→任务 done"永远不成立。
-- ---------------------------------------------------------------------------
INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
SELECT r.id, 'goal', 'open', jsonb_build_object('text', r.goal), 0, 'seed'
  FROM runs r
 WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.run_id = r.id AND n.kind = 'goal');

INSERT INTO edges (run_id, src_id, rel, dst_id)
SELECT g.run_id, o.id, 'spawns', g.id
  FROM nodes g
  JOIN LATERAL (
        SELECT id FROM nodes
         WHERE run_id = g.run_id AND kind = 'fact' AND state = 'origin'
         LIMIT 1
  ) o ON TRUE
 WHERE g.kind = 'goal' AND g.origin = 'seed'
   AND NOT EXISTS (
        SELECT 1 FROM edges e
         WHERE e.run_id = g.run_id AND e.dst_id = g.id AND e.rel = 'spawns'
   );

-- ---------------------------------------------------------------------------
-- 13. M3：计划图（plan / step）与 satisfies / realized_by / depends_on 边
--     约束升级：先检测再重建（PG 不支持给 CHECK 加枚举值）
-- ---------------------------------------------------------------------------
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'nodes'::regclass
             AND conname = 'ck_node_kind'
             AND pg_get_constraintdef(oid) NOT LIKE '%step%') THEN
    ALTER TABLE nodes DROP CONSTRAINT ck_node_kind;
    ALTER TABLE nodes ADD CONSTRAINT ck_node_kind
      CHECK (kind IN ('goal','intent','fact','finding','hint','digest','plan','step'));
  END IF;
END $$;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'nodes'::regclass
             AND conname = 'ck_node_state'
             AND pg_get_constraintdef(oid) NOT LIKE '%skipped%') THEN
    ALTER TABLE nodes DROP CONSTRAINT ck_node_state;
    ALTER TABLE nodes ADD CONSTRAINT ck_node_state CHECK (
        (kind = 'intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind = 'goal'    AND state IN ('open','met','abandoned')) OR
        (kind = 'fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind = 'finding' AND state IN ('confirmed','dismissed')) OR
        (kind = 'hint'    AND state IN ('active','consumed')) OR
        (kind = 'digest'  AND state IN ('active','superseded')) OR
        (kind = 'plan'    AND state IN ('active','done','abandoned')) OR
        (kind = 'step'    AND state IN ('todo','ready','running','done','failed','skipped'))
    );
  END IF;
END $$;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'edges'::regclass
             AND conname = 'ck_edge_rel'
             AND pg_get_constraintdef(oid) NOT LIKE '%satisfies%') THEN
    ALTER TABLE edges DROP CONSTRAINT ck_edge_rel;
    ALTER TABLE edges ADD CONSTRAINT ck_edge_rel
      CHECK (rel IN ('spawns','derived_from','yields','proves','covers','satisfies','realized_by','depends_on'));
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_steps_state ON nodes(run_id, state) WHERE kind = 'step';

-- ---------------------------------------------------------------------------
-- 14. M3.5：目标拆解标记 + 差分态势的版本戳
--
-- goals_at：第 0 轮 goals 拆解完成的时刻（0 = 还没拆过）。拆解失败不写，
--           下一轮会重试；此时 CreateRun 种下的 seed 目标仍然兜底，任务不会停摆。
--
-- rev：这一行"最后一次被写入时分配的版本号"。版本由 runs.version 单调分配
--      （见 store/delta.go: bumpVersion），于是"自版本 V 以来的变化"= rev > V。
--      历史行的 rev 留 0：它们属于首次全量快照，不该再出现在增量里。
-- ---------------------------------------------------------------------------
ALTER TABLE run_tasks ADD COLUMN IF NOT EXISTS goals_at BIGINT NOT NULL DEFAULT 0;

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS rev BIGINT NOT NULL DEFAULT 0;
ALTER TABLE edges ADD COLUMN IF NOT EXISTS rev BIGINT NOT NULL DEFAULT 0;

-- seeds：用户在 --target 里给的入口 URL（可能带端口/路径）。
-- 只把 target 降成"范围"会丢掉端口，worker 就只能猜端口 —— 真实 run 里 49% 的 HTTP
-- 请求打在 80/443/3000/8000… 上。范围回答"允许打哪"，seeds 回答"从哪开始"。
ALTER TABLE runs ADD COLUMN IF NOT EXISTS seeds TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_nodes_rev ON nodes(run_id, rev);
CREATE INDEX IF NOT EXISTS idx_edges_rev ON edges(run_id, rev);

-- ---------------------------------------------------------------------------
-- M6a：审批门（规则 → 可选判官 → 人工挂起）
--
-- 设计取舍：规则进 DB（可停用可审计），默认策略显式存在（catch-all allow），
-- 于是"为什么放行"永远能回答；破坏性 HTTP 方法默认 ask，不打断正常探测。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS intercept_rules (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT REFERENCES runs(id) ON DELETE CASCADE,   -- 空 = 全局
    priority    INT NOT NULL DEFAULT 100,
    match_tool  TEXT NOT NULL DEFAULT '',                        -- 空 = 任意工具
    match_regex TEXT NOT NULL DEFAULT '',                        -- 空 = 任意参数
    decision    TEXT NOT NULL CHECK (decision IN ('allow','deny','ask')),
    note        TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_intercept_rules_lookup
    ON intercept_rules(priority DESC, id DESC) WHERE enabled;

CREATE TABLE IF NOT EXISTS intercept_pending (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    intent_id   BIGINT,
    tool        TEXT NOT NULL,
    tool_use_id TEXT NOT NULL DEFAULT '',
    tool_input  JSONB NOT NULL DEFAULT '{}'::jsonb,
    rule_id     BIGINT,
    reason      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','allowed','denied','timeout')),
    decided_by  TEXT NOT NULL DEFAULT '',
    decided_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending(status, id DESC);

-- 默认规则（只在表为空时种）：catch-all allow 放在最低优先级，保证"每条放行都有规则可归因"。
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM intercept_rules) THEN
    INSERT INTO intercept_rules (priority, match_tool, match_regex, decision, note) VALUES
      (100, 'http_request', '(?i)"method"\s*:\s*"DELETE"', 'ask', 'DELETE 是破坏性请求：需要人工审批'),
      (90,  'http_request', '(?i)"method"\s*:\s*"(PUT|PATCH)"', 'ask', 'PUT/PATCH 会改数据：需要人工审批'),
      (0,   '', '', 'allow', '默认策略：放行（显式存在，便于审计"为什么放行"）');
  END IF;
END $$;

-- ---------------------------------------------------------------------------
-- M6b：流量录制（在唯一出网点 http_request 里录，不架 MITM）
--
-- 为什么不是代理：worker 的工具面里没有任意命令执行，所有目标流量都由本进程的
-- http_request 产生。在工具里录能拿到 intent 归属（代理做不到），也不需要 CA。
-- 正文：≤8KB 内联（可搜），更大落内容寻址文件，只留 sha + 预览。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS traffic_exchanges (
    id           BIGSERIAL PRIMARY KEY,
    run_id       BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    intent_id    BIGINT,
    tool_use_id  TEXT NOT NULL DEFAULT '',
    ts           TIMESTAMPTZ NOT NULL DEFAULT now(),
    method       TEXT NOT NULL DEFAULT 'GET',
    url          TEXT NOT NULL,
    url_template TEXT NOT NULL DEFAULT '',
    status       INT NOT NULL DEFAULT 0,
    content_type TEXT NOT NULL DEFAULT '',
    req_len      INT NOT NULL DEFAULT 0,
    resp_len     INT NOT NULL DEFAULT 0,
    duration_ms  INT NOT NULL DEFAULT 0,
    req_headers  JSONB NOT NULL DEFAULT '{}'::jsonb,
    resp_headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    req_body     TEXT NOT NULL DEFAULT '',      -- 预览（≤8KB）
    resp_body    TEXT NOT NULL DEFAULT '',
    req_sha      TEXT NOT NULL DEFAULT '',
    resp_sha     TEXT NOT NULL DEFAULT '',
    req_path     TEXT NOT NULL DEFAULT '',      -- 相对 dataDir；空 = 未落盘
    resp_path    TEXT NOT NULL DEFAULT '',
    is_binary    BOOLEAN NOT NULL DEFAULT false,
    truncated    BOOLEAN NOT NULL DEFAULT false,
    error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_traffic_run ON traffic_exchanges(run_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_traffic_run_ts ON traffic_exchanges(run_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_traffic_run_host ON traffic_exchanges(run_id, url_template);

-- 证据绑定：绑定时把预览快照下来，删除流量不影响证据（与 ARTEX 的 evidence 同思路）。
CREATE TABLE IF NOT EXISTS finding_traffic (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    finding_id  BIGINT NOT NULL,
    exchange_id BIGINT,                        -- 允许为空：流量删了，绑定还在
    method      TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    status      INT NOT NULL DEFAULT 0,
    req_sha     TEXT NOT NULL DEFAULT '',
    resp_sha    TEXT NOT NULL DEFAULT '',
    req_preview TEXT NOT NULL DEFAULT '',
    resp_preview TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_finding_traffic ON finding_traffic(run_id, finding_id);

-- 全文检索：有 pg_trgm 就建 GIN（子串可搜、中文可用），没有就跳过（退化成 ILIKE）。
--
-- 两个坑：① 扩展装在 public，而测试 schema 的 search_path 里没有 public →
-- 操作符类要显式写 public.gin_trgm_ops；② 没权限装扩展时不能让迁移整体失败。
DO $$
DECLARE
  stmt text;
BEGIN
  BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public;
  EXCEPTION WHEN OTHERS THEN
    NULL;
  END;
  FOREACH stmt IN ARRAY ARRAY[
    'CREATE INDEX IF NOT EXISTS idx_traffic_resp_trgm ON traffic_exchanges USING gin (resp_body public.gin_trgm_ops)',
    'CREATE INDEX IF NOT EXISTS idx_traffic_req_trgm ON traffic_exchanges USING gin (req_body public.gin_trgm_ops)',
    'CREATE INDEX IF NOT EXISTS idx_traffic_resp_trgm ON traffic_exchanges USING gin (resp_body gin_trgm_ops)',
    'CREATE INDEX IF NOT EXISTS idx_traffic_req_trgm ON traffic_exchanges USING gin (req_body gin_trgm_ops)'
  ] LOOP
    BEGIN
      EXECUTE stmt;
    EXCEPTION WHEN OTHERS THEN
      NULL; -- 这一种写法不可用就试下一种；都不行就退化成 ILIKE
    END;
  END LOOP;
END $$;


-- ---------------------------------------------------------------------------
-- M6c：finding 复测
--
-- 复测 = 用 finding 绑定的那条交换重放一次，按断言给 verified / failed / unverifiable。
-- 不能只回"看起来还在"：状态码、响应体指纹（sha/长度）都要留 expected/observed 供复核。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS finding_retests (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    finding_id  BIGINT NOT NULL,
    exchange_id BIGINT,
    status      TEXT NOT NULL CHECK (status IN ('verified','failed','unverifiable')),
    expected    TEXT NOT NULL DEFAULT '',
    observed    TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    duration_ms INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_retests_finding ON finding_retests(run_id, finding_id, id DESC);

-- ---------------------------------------------------------------------------
-- P2-5：trace / span（把"一次执行"串成时间线）
--
-- 取舍：**不建 spans 表**，span 直接落在 activity 上。理由是单一发射点：
-- 落库 + 推流 + 留痕必须走同一条路，否则时间线与过程记录一定会对不上。
--
-- trace_id 是**确定性**的（qy-<runID>），不是每次启动随机：
-- 重启后要给同一个任务补 span，随机会让旧 span 与新的断成两条时间线。
-- span 行 kind='span'，attrs 放 metadata，只把"要查询/排序的"提成真列：
-- span_name（按名字聚合 p99）、duration_ms（慢请求定位）、parent_span_id（还原层级）。
-- ---------------------------------------------------------------------------
ALTER TABLE activity ADD COLUMN IF NOT EXISTS trace_id       TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN IF NOT EXISTS span_id        TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN IF NOT EXISTS parent_span_id TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN IF NOT EXISTS span_name      TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN IF NOT EXISTS duration_ms    INT  NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_act_span      ON activity(run_id, id) WHERE span_id <> '';
CREATE INDEX IF NOT EXISTS idx_act_span_name ON activity(run_id, span_name, duration_ms) WHERE span_id <> '';

-- P2-3：outbox（写库与"写库后必须发生的副作用"同一个事务）
--
-- 为什么必须有它：finding 入库了但通知没发（或发两次）、任务删了但外部系统还留着单，
-- 这类问题都来自"两处代码各写各的"。outbox 把它们变成一个事务里的两行：
-- 业务行 + 事件行；提交后就一定有人会去投递（重启/停机都不丢）。
--
-- 幂等键 (topic, payload_hash)：同一个事件重复入队只留一行。
-- 这比"调用方自觉只发一次"可靠 —— 重试层会重放整条意图（L4）。
--
-- 领用用 FOR UPDATE SKIP LOCKED + 租约（locked_until），所以多实例并发安全。
-- activity 的 SSE 广播不走 outbox：它要求低延迟且可丢（与 ARTEX 同一取舍）。
CREATE TABLE IF NOT EXISTS outbox (
    id           BIGSERIAL PRIMARY KEY,
    run_id       BIGINT REFERENCES runs(id) ON DELETE CASCADE,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload_hash TEXT NOT NULL DEFAULT '',
    attempts     INT NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until TIMESTAMPTZ,
    done_at      TIMESTAMPTZ,
    dead_at      TIMESTAMPTZ,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_outbox_event ON outbox(topic, payload_hash);
CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox(available_at, id)
    WHERE done_at IS NULL AND dead_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_outbox_dead ON outbox(dead_at DESC) WHERE dead_at IS NOT NULL;

-- P3-4：审批策略（预授权 / 批量 / 按风险分级的超时）
--
-- 与 intercept_rules 的区别：规则只回答"要不要问人"，策略还要回答
-- "能不能不问人就直接放行（预授权）"以及"放行到什么程度为止（限速/到期）"。
--
-- 判定顺序（两段式，deny 不可被覆盖）：
--   1) 任何启用的 deny 策略命中 → deny（连规则里的 allow 也盖不住）
--   2) 规则里的 deny 命中 → deny
--   3) 其余策略按 priority 降序取第一条 → allow/ask；allow 受 max_per_hour 限制
--   4) 没命中 → 回到规则表（老行为，catch-all allow 兜底）
--
-- approval_grants 既是审计（每条预授权放行都留一行）也是限速的计数来源：
-- 用"最近一小时放过几次"来判 max_per_hour，不需要在内存里记窗口。
CREATE TABLE IF NOT EXISTS approval_policies (
    id           BIGSERIAL PRIMARY KEY,
    run_id       BIGINT REFERENCES runs(id) ON DELETE CASCADE,   -- 空 = 全局
    priority     INT NOT NULL DEFAULT 100,
    match_tool   TEXT NOT NULL DEFAULT '',
    match_regex  TEXT NOT NULL DEFAULT '',
    match_scope  TEXT NOT NULL DEFAULT 'any'
                 CHECK (match_scope IN ('same_target','in_scope','any')),
    decision     TEXT NOT NULL CHECK (decision IN ('allow','deny','ask')),
    max_per_hour INT,
    expires_at   TIMESTAMPTZ,
    note         TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_approval_policies_lookup
    ON approval_policies(priority DESC, id DESC) WHERE enabled;

CREATE TABLE IF NOT EXISTS approval_grants (
    id         BIGSERIAL PRIMARY KEY,
    policy_id  BIGINT NOT NULL REFERENCES approval_policies(id) ON DELETE CASCADE,
    run_id     BIGINT NOT NULL,
    tool       TEXT NOT NULL DEFAULT '',
    target     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_approval_grants_rate ON approval_grants(policy_id, created_at DESC);

-- P3-1：战役（campaign）= run 的父容器
--
-- 存在的理由不是"分组好看"，而是三个只有跨 run 才能回答的问题：
--   1) 总覆盖度：战役范围里的实体，被任一子 run 的 fact 锚定过的比例；
--   2) 总进度：所有子 run 的目标状态聚合；
--   3) 重复投入：同一实体被多个子 run 测过 → 该拆分/该合并的信号。
--
-- 范围只允许"减"不允许"加"：子 run 继承战役范围，但战役加宽必须人显式确认
-- （升级计划 P3-1 的风险缓解）。
-- 删除战役**不级联删 run**（ON DELETE SET NULL）：先解绑再删，避免误删证据。
CREATE TABLE IF NOT EXISTS campaigns (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    goal       TEXT NOT NULL,
    org_ids    BIGINT[] NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','paused','done','archived')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE runs ADD COLUMN IF NOT EXISTS campaign_id BIGINT REFERENCES campaigns(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS campaign_role TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_runs_campaign ON runs(campaign_id) WHERE campaign_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS campaign_scope (
    id          BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('root_domain','subdomain','ip','cidr','org','icp','keyword')),
    domain      TEXT,
    net         CIDR,
    org_id      BIGINT,
    value       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_campaign_scope ON campaign_scope(campaign_id, kind,
    COALESCE(domain, ''), COALESCE(net::text, ''), COALESCE(org_id, 0), COALESCE(value, ''));

-- P3-3：连接器投递台账（幂等键 = connector + event_key）
--
-- 为什么要单独一张表：outbox 保证"至少一次"，外部系统需要"恰好一次"。
-- 台账记下"这个事件已经推给这个连接器、外部 id 是什么"，重投时直接跳过。
-- 它与 outbox 的关系：outbox 是"要送出去什么"，台账是"已经送到哪、对面怎么叫它"。
CREATE TABLE IF NOT EXISTS connector_deliveries (
    id          BIGSERIAL PRIMARY KEY,
    connector   TEXT NOT NULL,
    event_key   TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pushed',
    last_error  TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_connector_delivery ON connector_deliveries(connector, event_key);
CREATE INDEX IF NOT EXISTS idx_connector_delivery_ext ON connector_deliveries(connector, external_id)
    WHERE external_id <> '';

-- ---------------------------------------------------------------------------
-- M12：任务附件（只读投喂）
--
-- 取舍：附件**不是**给 agent 的任意文件系统权限，而是"每任务一组只读材料"：
--   - 内容按 sha256 存在 data/runs/<run>/attachments/ 下（同一任务内同内容只存一份）；
--   - agent 只能通过 list_attachments / read_attachment 读，**没有** bash、没有路径参数；
--   - summary 是"前 200 字摘要"，注入态势用（模型先知道有什么，再决定读哪个）。
-- 附件内容属于不可信输入（I18）：工具返回会带"不可信"标记，prompt 也这么写。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS attachments (
    id         BIGSERIAL PRIMARY KEY,
    run_id     BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,                      -- 展示名（已清洗，去掉路径分隔符）
    kind       TEXT NOT NULL DEFAULT 'text'
               CHECK (kind IN ('text','binary','archive','image')),
    mime       TEXT NOT NULL DEFAULT '',
    size       BIGINT NOT NULL DEFAULT 0,
    sha256     TEXT NOT NULL,
    path       TEXT NOT NULL DEFAULT '',           -- 相对 dataDir
    summary    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, name)
);
CREATE INDEX IF NOT EXISTS idx_attachments_run ON attachments(run_id, id);