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

CREATE INDEX IF NOT EXISTS idx_nodes_rev ON nodes(run_id, rev);
CREATE INDEX IF NOT EXISTS idx_edges_rev ON edges(run_id, rev);
