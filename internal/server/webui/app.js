/* 钦原控制台 · 仪表盘（单页 + hash 路由）
 *
 * 信息架构参考 ARTEX：侧边导航 → 页面（仪表盘 / 任务 / 发现 / 资产 / 设置），
 * 任务详情是多 Tab（概览 / 会话 / 计划 / 成本 / 覆盖）。
 *
 * 三条数据纪律（实现规格/06 §6）：
 *  1. 会话 = 历史 ∪ 实时，按 id 去重；广播可丢，DB 是完整性来源；
 *  2. 轮询只拉"态势"，活动流一律走 SSE；
 *  3. 结构签名相同就不重渲染（避免每秒闪）。
 */

const $ = (id) => document.getElementById(id);

const S = {
  stats: null,
  runs: [],
  findings: [],
  assets: [],
  health: null,
  run: null,
  overview: null,
  steps: [],
  tokens: null,
  activities: [],
  seen: new Set(),
  toolCards: new Map(),
  lastId: 0,
  es: null,
  connected: false,
  stickBottom: true,
  filter: 'all',
  statusFilter: 'all',
  search: '',
  theme: localStorage.getItem('qy_theme') || 'light',
  route: null,
  refresh: null,
  cleanup: null,
  sig: {},
};

/* ── 小工具 ─────────────────────────────────────────── */

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
}
function h(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}
const fmtInt = (n) => (n == null ? '–' : Number(n).toLocaleString('en-US'));
const fmtTok = (n) => (n == null ? '–' : n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n));
const fmtTime = (unix) => {
  if (!unix) return '—';
  const d = new Date(unix * 1000);
  return [d.getHours(), d.getMinutes(), d.getSeconds()].map((v) => String(v).padStart(2, '0')).join(':');
};
const fmtDate = (unix) => {
  if (!unix) return '—';
  const d = new Date(unix * 1000);
  return `${d.getMonth() + 1}/${d.getDate()} ${fmtTime(unix)}`;
};
const ago = (unix) => {
  if (!unix) return '—';
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
};

const STATUS_LABEL = {
  running: '运行中', paused: '已暂停', queued: '排队中', done: '已完成',
  failed: '失败', timeout: '超时', created: '已创建', idle: '空闲',
};
const statusBadge = (st) => `<span class="badge badge-${st === 'created' ? 'idle' : st || 'idle'}">${STATUS_LABEL[st] || st || '未知'}</span>`;

async function api(path, init) {
  const res = await fetch(path, init);
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { raw: text }; }
  if (!res.ok) throw new Error((body && body.error) || `HTTP ${res.status}`);
  return body;
}

function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { t.hidden = true; }, 2400);
}

function empty(title, hint) {
  return `<div class="empty"><strong>${title}</strong>${hint || ''}</div>`;
}

/* ── 主题 / 顶栏 ─────────────────────────────────────── */

function applyTheme() {
  document.documentElement.dataset.theme = S.theme;
  localStorage.setItem('qy_theme', S.theme);
}

function setPage(title, sub) {
  $('page-title').textContent = title;
  if (sub != null) $('page-sub').textContent = sub;
  document.title = title ? `${title} · 钦原` : '钦原 · 控制台';
}

function markNav() {
  const path = location.hash.split('?')[0] || '#/dashboard';
  document.querySelectorAll('.nav-item').forEach((a) => {
    const m = a.dataset.match;
    const on = path === m || (m !== '#/dashboard' && path.startsWith(m));
    a.classList.toggle('is-active', on);
  });
}

async function pollGlobal() {
  try {
    const [st, health] = await Promise.all([api('/api/stats'), api('/health')]);
    S.stats = st;
    S.health = health;
    $('health-dot').className = 'dot is-ok';
    $('health-text').textContent = `后端就绪 · v${health.version}`;
    $('engine-chip').textContent = health.llm_configured ? '模型已配置' : '模型：fake/未配置';
    $('badge-runs').textContent = st.running ? String(st.running) : '';
    $('badge-findings').textContent = st.findings ? String(st.findings) : '';
    $('badge-assets').textContent = st.entities ? String(st.entities) : '';
  } catch (e) {
    $('health-dot').className = 'dot is-bad';
    $('health-text').textContent = '后端不可用';
  }
}

/* ── 路由 ────────────────────────────────────────────── */

function parseHash() {
  const raw = location.hash.replace(/^#/, '') || '/dashboard';
  const [path, qs] = raw.split('?');
  const seg = path.split('/').filter(Boolean);
  const q = new URLSearchParams(qs || '');
  if (seg[0] === 'runs' && seg[1]) return { name: 'run', runId: Number(seg[1]), tab: seg[2] || q.get('tab') || 'overview' };
  if (seg[0] === 'runs') return { name: 'runs' };
  if (seg[0] === 'findings') return { name: 'findings' };
  if (seg[0] === 'assets') return { name: 'assets' };
  if (seg[0] === 'settings') return { name: 'settings' };
  return { name: 'dashboard' };
}

async function route() {
  if (S.cleanup) { S.cleanup(); S.cleanup = null; }
  if (S.es) { S.es.close(); S.es = null; }
  S.refresh = null;
  S.route = parseHash();
  markNav();
  $('view').innerHTML = `<div class="skeleton"><span class="spinner"></span> 加载中…</div>`;
  try {
    switch (S.route.name) {
      case 'runs': await renderRuns(); break;
      case 'run': await renderRun(); break;
      case 'findings': await renderFindings(); break;
      case 'assets': await renderAssets(); break;
      case 'settings': await renderSettings(); break;
      default: await renderDashboard();
    }
  } catch (e) {
    $('view').innerHTML = empty('加载失败', e.message);
  }
}

/* ── 仪表盘 ──────────────────────────────────────────── */

async function renderDashboard() {
  setPage('仪表盘', '任务、事实与成本的总览');
  const [runs, stats] = await Promise.all([api('/api/runs'), api('/api/stats')]);
  S.runs = runs.items || [];
  S.stats = stats;

  const effective = Math.max(0, stats.input_tokens - stats.cache_read_tokens);
  const cachePct = stats.input_tokens ? Math.round((stats.cache_read_tokens / stats.input_tokens) * 100) : 0;
  const recent = S.runs.slice(0, 6);
  const latest = S.runs.find((r) => r.status === 'running') || S.runs[0];

  const view = $('view');
  view.innerHTML = `
    <div class="stack">
      <div class="grid grid-kpi">
        <div class="card kpi">
          <div class="kpi-label">任务</div>
          <div class="kpi-value">${fmtInt(stats.runs)}<small>个</small></div>
          <div class="kpi-sub"><b>${stats.running}</b> 运行中 · <b>${stats.done}</b> 已完成</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">事实</div>
          <div class="kpi-value">${fmtInt(stats.facts)}<small>条</small></div>
          <div class="kpi-sub">来自 <b>${stats.intents}</b> 条意图</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">发现</div>
          <div class="kpi-value">${fmtInt(stats.findings)}<small>个</small></div>
          <div class="kpi-sub">资产 <b>${fmtInt(stats.entities)}</b> 个</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">有效输入</div>
          <div class="kpi-value">${fmtTok(effective)}<small>tok</small></div>
          <div class="kpi-sub">缓存命中 <b>${cachePct}%</b> · 输出 ${fmtTok(stats.output_tokens)}</div>
        </div>
      </div>

      <div class="grid grid-2">
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">最近任务</h2>
            <a class="btn btn-sm" href="#/runs">全部任务</a>
          </div>
          ${recent.length ? `<table class="table">
            <thead><tr><th>状态</th><th>目标</th><th class="num">轮次</th><th>更新</th></tr></thead>
            <tbody>${recent.map((r) => `
              <tr class="is-clickable" data-run="${r.id}">
                <td>${statusBadge(r.status)}</td>
                <td class="goal-cell"><span>${esc(r.goal)}</span></td>
                <td class="num">${r.round ?? 0}</td>
                <td class="muted">${ago(r.updated_at)}</td>
              </tr>`).join('')}
            </tbody></table>` : empty('还没有任务', '用 CLI 或 API 建一个：<code>qinyuan run create --goal …</code>')}
        </div>

        <div class="stack">
          <div class="card">
            <div class="card-head"><h2 class="card-title">任务状态分布</h2></div>
            <div class="card-body stack" style="gap:10px">
              ${[
                ['running', stats.running, '运行中'],
                ['paused', stats.paused, '已暂停'],
                ['queued', stats.queued, '排队中'],
                ['done', stats.done, '已完成'],
                ['failed', stats.failed, '失败/超时'],
              ].map(([k, v, label]) => {
                const pct = stats.runs ? Math.round((v / stats.runs) * 100) : 0;
                const color = k === 'running' ? 'var(--info)'
                  : k === 'paused' ? 'var(--warn)'
                  : k === 'done' ? 'var(--ok)'
                  : k === 'failed' ? 'var(--danger)' : 'var(--accent)';
                return `<div class="bar-row">
                  <span class="bar-label">${label}</span>
                  <span class="bar-track"><span class="bar-fill" style="width:${pct}%;background:${color}"></span></span>
                  <span class="bar-value">${v}</span>
                </div>`;
              }).join('')}
            </div>
          </div>

          <div class="card">
            <div class="card-head"><h2 class="card-title">最近活动<small>${latest ? `#${latest.id}` : ''}</small></h2>
              ${latest ? `<a class="btn btn-sm" href="#/runs/${latest.id}/sessions">打开会话</a>` : ''}
            </div>
            <div class="card-body tight row-list scroll" id="dash-activity"><div class="skeleton">加载…</div></div>
          </div>
        </div>
      </div>
    </div>`;

  view.querySelectorAll('[data-run]').forEach((row) => {
    row.addEventListener('click', () => { location.hash = `#/runs/${row.dataset.run}`; });
  });

  if (latest) {
    try {
      const data = await api(`/api/runs/${latest.id}/activity?limit=12`);
      const items = (data.items || []).filter((a) => a.kind !== 'usage').slice(-7);
      const box = $('dash-activity');
      box.innerHTML = items.length ? items.map((a) => `
        <div class="row-item">
          <span class="state-dot ${a.is_error ? 'failed' : ''}"></span>
          <div class="row-main">
            <div class="row-title clamp1">${esc(a.summary || a.tool || a.kind)}${a.is_error ? ' · 失败' : ''}</div>
            <div class="row-meta"><span>${a.tool || a.kind}</span><span>${fmtTime(a.created_at)}</span><span>#${a.id}</span></div>
          </div>
        </div>`).join('') : empty('还没有活动');
    } catch { /* 忽略 */ }
  }

  S.refresh = async () => {
    const [r2, s2] = await Promise.all([api('/api/runs'), api('/api/stats')]);
    S.runs = r2.items || [];
    S.stats = s2;
  };
}

/* ── 任务列表 ────────────────────────────────────────── */

async function renderRuns() {
  setPage('任务', '全部探索任务');
  const data = await api('/api/runs');
  S.runs = data.items || [];
  drawRuns();
  S.refresh = async () => { S.runs = (await api('/api/runs')).items || []; drawRuns(); };
}

function drawRuns() {
  const st = S.statusFilter;
  const q = S.search.trim().toLowerCase();
  const items = S.runs.filter((r) => {
    if (st !== 'all' && r.status !== st) return false;
    if (q && !(r.goal || '').toLowerCase().includes(q)) return false;
    return true;
  });

  $('view').innerHTML = `
    <div class="toolbar">
      <div class="seg" id="status-seg">
        ${[['all', '全部'], ['running', '运行中'], ['paused', '已暂停'], ['done', '已完成']]
          .map(([k, label]) => `<button data-st="${k}" class="${st === k ? 'is-on' : ''}">${label}</button>`).join('')}
      </div>
      <input class="search" id="run-search" placeholder="搜索目标…" value="${esc(S.search)}" />
      <span class="spacer"></span>
      <span class="muted">${items.length} / ${S.runs.length} 个任务</span>
      <button class="btn" id="run-refresh">刷新</button>
    </div>
    <div class="card">
      ${items.length ? `<table class="table">
        <thead><tr>
          <th>状态</th><th>目标</th><th class="num">轮次</th><th class="num">意图</th>
          <th>创建</th><th>更新</th><th></th>
        </tr></thead>
        <tbody>${items.map(runRow).join('')}</tbody>
      </table>` : empty('没有匹配的任务', q ? '换个关键词试试。' : '还没有任务。')}
    </div>
    <p class="page-foot">点一行打开详情：概览 / 会话（实时）/ 计划 / 成本 / 覆盖。</p>`;

  $('status-seg').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-st]');
    if (!b) return;
    S.statusFilter = b.dataset.st;
    drawRuns();
  });
  $('run-search').addEventListener('input', (e) => {
    S.search = e.target.value;
    clearTimeout(drawRuns._t);
    drawRuns._t = setTimeout(drawRuns, 180);
  });
  $('run-refresh').addEventListener('click', () => S.refresh && S.refresh());

  document.querySelectorAll('[data-run]').forEach((row) => {
    row.addEventListener('click', (ev) => {
      if (ev.target.closest('button')) return;
      location.hash = `#/runs/${row.dataset.run}`;
    });
  });
  document.querySelectorAll('[data-act]').forEach((btn) => {
    btn.addEventListener('click', (ev) => {
      ev.stopPropagation();
      control(Number(btn.dataset.run), btn.dataset.act);
    });
  });
}

function runRow(r) {
  const intents = (r.intents || []).length;
  return `<tr class="is-clickable" data-run="${r.id}">
    <td>${statusBadge(r.status)}</td>
    <td class="goal-cell"><span>${esc(r.goal)}</span></td>
    <td class="num">${r.round ?? 0}</td>
    <td class="num">${intents || '—'}</td>
    <td class="muted">${fmtDate(r.created_at)}</td>
    <td class="muted">${ago(r.updated_at)}</td>
    <td class="actions">
      ${r.can_pause ? `<button class="btn btn-sm" data-act="pause" data-run="${r.id}">暂停</button>` : ''}
      ${r.can_resume ? `<button class="btn btn-sm" data-act="resume" data-run="${r.id}">恢复</button>` : ''}
      <button class="btn btn-sm" data-act="open" data-run="${r.id}">详情</button>
    </td>
  </tr>`;
}

async function control(id, action) {
  if (action === 'open') { location.hash = `#/runs/${id}`; return; }
  try {
    await api(`/api/runs/${id}/control`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action }),
    });
    toast(action === 'pause' ? '已暂停' : '已恢复');
    if (S.refresh) await S.refresh();
    await pollGlobal();
  } catch (e) { toast('操作失败：' + e.message); }
}

/* ── 任务详情 ────────────────────────────────────────── */

async function renderRun() {
  const id = S.route.runId;
  const tab = S.route.tab;
  const [run, overview] = await Promise.all([
    api(`/api/runs/${id}`),
    api(`/api/runs/${id}/overview`).catch(() => null),
  ]);
  S.run = run;
  S.overview = overview;
  setPage(`任务 #${id}`, run.goal);

  const tabs = [['overview', '概览'], ['sessions', '会话'], ['plan', '计划'], ['cost', '成本'], ['coverage', '覆盖']];
  $('view').innerHTML = `
    <div class="stack">
      <div class="card">
        <div class="detail-head">
          <h2>${esc(run.goal)}</h2>
          <div class="detail-meta">
            <span>${statusBadge(run.status)}</span>
            <span>轮次 <b>${run.round ?? 0}</b></span>
            <span>工作线程 <b>${run.worker_count ?? '—'}</b></span>
            <span>超时 <b>${run.timeout_seconds ? run.timeout_seconds + 's' : '不限'}</b></span>
            <span>创建 <b>${fmtDate(run.created_at)}</b></span>
          </div>
          <div class="detail-actions">
            ${run.can_pause ? `<button class="btn" data-act="pause">暂停</button>` : ''}
            ${run.can_resume ? `<button class="btn" data-act="resume">恢复</button>` : ''}
            <a class="btn btn-ghost" href="#/runs">返回列表</a>
          </div>
        </div>
        <div class="tabs" id="run-tabs">
          ${tabs.map(([k, label]) => `<button class="tab ${k === tab ? 'is-active' : ''}" data-tab="${k}">${label}</button>`).join('')}
        </div>
        <div id="tab-body"></div>
      </div>
    </div>`;

  $('run-tabs').addEventListener('click', (e) => {
    const b = e.target.closest('[data-tab]');
    if (!b) return;
    location.hash = `#/runs/${id}/${b.dataset.tab}`;
  });
  document.querySelectorAll('.detail-actions [data-act]').forEach((b) => {
    b.addEventListener('click', () => control(id, b.dataset.act));
  });

  await drawTab(id, tab);

  S.refresh = async () => {
    try {
      const [r, ov] = await Promise.all([
        api(`/api/runs/${id}`),
        api(`/api/runs/${id}/overview`).catch(() => S.overview),
      ]);
      S.run = r; S.overview = ov;
      const head = document.querySelector('.detail-head');
      if (head) {
        head.querySelector('.detail-meta').innerHTML = `
          <span>${statusBadge(r.status)}</span>
          <span>轮次 <b>${r.round ?? 0}</b></span>
          <span>工作线程 <b>${r.worker_count ?? '—'}</b></span>
          <span>超时 <b>${r.timeout_seconds ? r.timeout_seconds + 's' : '不限'}</b></span>
          <span>创建 <b>${fmtDate(r.created_at)}</b></span>`;
        const acts = head.querySelector('.detail-actions');
        acts.innerHTML = `${r.can_pause ? '<button class="btn" data-act="pause">暂停</button>' : ''}
          ${r.can_resume ? '<button class="btn" data-act="resume">恢复</button>' : ''}
          <a class="btn btn-ghost" href="#/runs">返回列表</a>`;
        acts.querySelectorAll('[data-act]').forEach((b) => b.addEventListener('click', () => control(id, b.dataset.act)));
      }
      if (S.route.tab !== 'sessions') await drawTab(id, S.route.tab);
    } catch { /* 忽略单次失败 */ }
  };
}

async function drawTab(id, tab) {
  const body = $('tab-body');
  if (!body) return;
  if (tab === 'overview') return drawOverview(id, body);
  if (tab === 'sessions') return drawSessions(id, body);
  if (tab === 'plan') return drawPlan(id, body);
  if (tab === 'cost') return drawCost(id, body);
  if (tab === 'coverage') return drawCoverage(id, body);
}

async function drawOverview(id, body) {
  const [ov, findings, stepsData] = await Promise.all([
    S.overview ? Promise.resolve(S.overview) : api(`/api/runs/${id}/overview`),
    api(`/api/runs/${id}/nodes?kind=finding&limit=50`).catch(() => ({ items: [] })),
    api(`/api/runs/${id}/nodes?kind=step&limit=200`).catch(() => ({ items: [] })),
  ]);
  const goals = (ov.goals || []).filter((g) => g.state !== 'abandoned');
  const met = goals.filter((g) => g.state === 'met').length;
  const intents = [...(ov.running_intents || []), ...(ov.open_intents || [])];
  const steps = stepsData.items || [];
  const doneSteps = steps.filter((s) => s.state === 'done').length;
  const facts = ov.recent_facts || [];

  body.innerHTML = `
    <div class="card-body stack" style="gap:16px">
      <div class="grid grid-kpi">
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">目标</div>
          <div class="kpi-value">${met}<small>/ ${goals.length}</small></div>
          <div class="kpi-sub">已用证据证明</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">计划步骤</div>
          <div class="kpi-value">${doneSteps}<small>/ ${steps.length}</small></div>
          <div class="kpi-sub">规则自动完成 ${steps.filter((s) => s.payload && s.payload.satisfied_by_rule).length}</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">事实 / 发现</div>
          <div class="kpi-value">${fmtInt(ov.facts_total)}<small>/ ${(findings.items || []).length}</small></div>
          <div class="kpi-sub">前沿待领 ${ov.frontier_open ?? 0}</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">覆盖度</div>
          <div class="kpi-value">${ov.coverage && ov.coverage.pct != null ? Math.round(ov.coverage.pct) : 0}<small>%</small></div>
          <div class="kpi-sub">${ov.coverage ? `${ov.coverage.tested} / ${ov.coverage.total} 已测` : '未登记范围'}</div>
        </div>
      </div>

      <div class="grid grid-2-even">
        <div class="card">
          <div class="card-head"><h2 class="card-title">目标</h2></div>
          ${goals.length ? `<div class="row-list">${goals.map((g) => `
            <div class="row-item">
              <span class="state-dot ${g.state}"></span>
              <div class="row-main">
                <div class="row-title">${esc(g.summary)}</div>
                <div class="row-meta"><span>#${g.id}</span><span>${g.state === 'met' ? '已达成' : '进行中'}</span></div>
              </div>
            </div>`).join('')}</div>` : empty('还没有目标')}
        </div>

        <div class="card">
          <div class="card-head"><h2 class="card-title">在跑 / 待领意图<small>${intents.length}</small></h2></div>
          ${intents.length ? `<div class="row-list">${intents.map((it) => `
            <div class="row-item">
              <span class="state-dot ${it.state}"></span>
              <div class="row-main">
                <div class="row-title">${esc(it.summary)}</div>
                <div class="row-meta"><span>#${it.id}</span><span>${it.state}</span></div>
              </div>
            </div>`).join('')}</div>` : empty('当前没有在跑的意图')}
        </div>
      </div>

      <div class="grid grid-2-even">
        <div class="card">
          <div class="card-head"><h2 class="card-title">最近事实</h2></div>
          ${facts.length ? `<div class="row-list">${facts.slice(-6).reverse().map((f) => `
            <div class="row-item">
              <span class="state-dot done"></span>
              <div class="row-main">
                <div class="row-title">${esc(f.summary)}</div>
                <div class="row-meta"><span>#${f.id}</span><span>${f.confidence || ''}</span></div>
              </div>
            </div>`).join('')}</div>` : empty('还没有事实')}
        </div>
        <div class="card">
          <div class="card-head"><h2 class="card-title">授权范围</h2></div>
          <div class="card-body">
            ${(ov.scopes || []).length
              ? `<div class="stack" style="gap:8px">${ov.scopes.map((s) => `<code class="mono">${esc(s)}</code>`).join('')}</div>`
              : `<p class="muted">未登记范围（默认拒绝出网）。</p>`}
          </div>
        </div>
      </div>
    </div>`;
}

/* ── 会话（SSE） ─────────────────────────────────────── */

async function drawSessions(id, body) {
  body.innerHTML = `
    <div class="card-body" style="padding:0">
      <div class="toolbar" style="padding:12px 16px 0">
        <div class="seg" id="session-filter">
          <button data-f="all" class="is-on">全部</button>
          <button data-f="plan">规划</button>
          <button data-f="work">执行</button>
          <button data-f="system">系统</button>
        </div>
        <span class="spacer"></span>
        <span class="muted" id="stream-state"><span class="spinner"></span> 连接中…</span>
      </div>
      <div class="transcript" id="transcript"></div>
      <div class="transcript-foot" id="transcript-foot">历史 ∪ 实时，按 id 去重</div>
    </div>`;

  S.activities = []; S.seen.clear(); S.toolCards.clear(); S.lastId = 0;
  $('session-filter').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-f]');
    if (!b) return;
    [...$('session-filter').children].forEach((x) => x.classList.toggle('is-on', x === b));
    S.filter = b.dataset.f;
    document.querySelectorAll('.t-row').forEach((row) => {
      row.hidden = S.filter !== 'all' && row.dataset.cat !== S.filter;
    });
  });

  const hist = await api(`/api/runs/${id}/activity?limit=300`);
  for (const a of (hist.items || [])) appendActivity(a);
  $('transcript-foot').textContent = `${S.activities.length} 条 · 历史已加载`;

  const tr = $('transcript');
  tr.scrollTop = tr.scrollHeight;
  tr.addEventListener('scroll', () => {
    S.stickBottom = tr.scrollHeight - tr.scrollTop - tr.clientHeight < 120;
  });

  connectStream(id);
  S.cleanup = () => { if (S.es) { S.es.close(); S.es = null; } };
}

function catOf(a) {
  const w = a.worker || '';
  if (w === 'planner') return 'plan';
  if (w === 'system' || !w) return 'system';
  return 'work';
}

function connectStream(id) {
  if (S.es) S.es.close();
  const es = new EventSource(`/api/runs/${id}/activity/stream`);
  S.es = es;
  const setState = (text, busy) => {
    const n = $('stream-state');
    if (n) n.innerHTML = busy ? `<span class="spinner"></span> ${text}` : text;
  };
  es.addEventListener('activity', (ev) => {
    try {
      S.connected = true;
      setState(`实时 · 已连接`, false);
      appendActivity(JSON.parse(ev.data));
    } catch {}
  });
  es.onopen = () => { S.connected = true; setState('实时 · 已连接', false); };
  es.onerror = () => { S.connected = false; setState('重连中…', true); };
}

function appendActivity(a) {
  if (!a || S.seen.has(a.id) || a.kind === 'usage') return;
  S.seen.add(a.id);
  S.lastId = Math.max(S.lastId, a.id);
  S.activities.push(a);
  const box = $('transcript');
  if (!box) return;

  const cat = catOf(a);
  const row = el('div', `t-row t-${a.kind}`);
  row.dataset.cat = cat;
  row.dataset.id = String(a.id);
  if (a.is_error) row.classList.add('t-error');
  if (a.kind === 'round') row.classList.add('t-round');
  if (S.filter !== 'all' && S.filter !== cat) row.hidden = true;

  row.appendChild(el('span', 't-time', fmtTime(a.created_at)));
  row.appendChild(el('span', 't-node', a.node_id ? `#${a.node_id}` : '—'));

  if (a.kind === 'tool_use' || a.kind === 'tool_result') {
    const key = a.tool_use_id || `a${a.id}`;
    const entry = S.toolCards.get(key);
    if (a.kind === 'tool_result' && entry) {
      entry.card.classList.remove('is-pending');
      entry.card.appendChild(toolLine(a, true));
      maybeStick();
      return;
    }
    const wrap = el('div', 't-body');
    const card = el('div', 'tool' + (a.kind === 'tool_use' ? ' is-pending' : ''));
    card.appendChild(toolLine(a, false));
    if (a.kind === 'tool_use') S.toolCards.set(key, { card });
    wrap.appendChild(card);
    row.appendChild(wrap);
  } else {
    row.appendChild(el('div', 't-body', a.summary || ''));
    row.addEventListener('click', () => openDetail(a));
  }
  box.appendChild(row);
  maybeStick();
  const foot = $('transcript-foot');
  if (foot && S.connected) foot.textContent = `${S.activities.length} 条 · 实时`;
}

function toolLine(a, isResult) {
  const line = el('div', 'tool-line' + (isResult ? ' result' : '') + (a.is_error ? ' err' : ''));
  line.appendChild(el('span', 'tool-name', isResult ? 'result' : (a.tool || 'tool')));
  line.appendChild(el('span', 'tool-payload', a.summary || ''));
  line.addEventListener('click', () => openDetail(a));
  return line;
}

function maybeStick() {
  const v = $('transcript') || $('view');
  if (!v) return;
  const near = v.scrollHeight - v.scrollTop - v.clientHeight < 120;
  if (near || S.stickBottom) v.scrollTop = v.scrollHeight;
}

/* ── 计划 / 成本 / 覆盖 ──────────────────────────────── */

async function drawPlan(id, body) {
  const data = await api(`/api/runs/${id}/nodes?kind=step&limit=200`);
  const steps = (data.items || []).slice().sort((a, b) => a.id - b.id);
  const done = steps.filter((s) => s.state === 'done').length;
  body.innerHTML = `
    <div class="card-body">
      <div class="bar-row" style="grid-template-columns:120px minmax(0,1fr) 80px;margin-bottom:16px">
        <span class="bar-label">完成进度</span>
        <span class="bar-track"><span class="bar-fill" style="width:${steps.length ? (done / steps.length) * 100 : 0}%"></span></span>
        <span class="bar-value">${done}/${steps.length}</span>
      </div>
      ${steps.length ? `<table class="table">
        <thead><tr><th style="width:88px">状态</th><th>步骤</th><th>判据（expected_evidence）</th><th style="width:76px">判定</th></tr></thead>
        <tbody>${steps.map((s) => {
          const p = s.payload || {};
          const byRule = p.satisfied_by_rule === true;
          return `<tr>
            <td>${statusBadge(s.state === 'done' ? 'done' : s.state === 'running' ? 'running' : s.state === 'ready' ? 'queued' : 'idle').replace(/>[^<]*</, `>${s.state === 'done' ? '已完成' : s.state === 'running' ? '进行中' : s.state === 'ready' ? '可执行' : '待办'}<`)}</td>
            <td>${esc(p.text || s.summary || '')}</td>
            <td class="mono muted">${esc(p.expected_evidence || '')}</td>
            <td class="muted">${p.satisfied_by_fact ? (byRule ? '规则' : '人工') : '—'}</td>
          </tr>`;
        }).join('')}</tbody>
      </table>` : empty('还没有计划图', 'planner 会在第一轮拆步（add_plan_step）。')}
    </div>`;
}

async function drawCost(id, body) {
  const tk = await api(`/api/runs/${id}/tokens`);
  const by = tk.by_node || [];
  const max = Math.max(1, ...by.map((n) => n.effective_input_tokens));
  body.innerHTML = `
    <div class="card-body stack">
      <div class="grid grid-kpi">
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">名义输入</div>
          <div class="kpi-value">${fmtTok(tk.input_tokens)}</div>
          <div class="kpi-sub">含缓存命中部分</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">有效输入</div>
          <div class="kpi-value">${fmtTok(tk.effective_input_tokens)}</div>
          <div class="kpi-sub">input − cache_read</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">缓存命中</div>
          <div class="kpi-value">${Math.round(tk.cache_hit_pct || 0)}<small>%</small></div>
          <div class="kpi-sub">前缀缓存节省</div>
        </div>
        <div class="card kpi" style="box-shadow:none;background:var(--panel-2)">
          <div class="kpi-label">每条事实</div>
          <div class="kpi-value">${fmtTok(tk.effective_per_fact)}<small>tok</small></div>
          <div class="kpi-sub">${tk.facts} 条事实</div>
        </div>
      </div>

      ${by.length ? `<div class="card">
        <div class="card-head"><h2 class="card-title">按节点</h2><span class="muted">node 0 = planner / goals</span></div>
        <table class="table">
          <thead><tr><th>节点</th><th class="num">调用</th><th>有效输入</th><th class="num">输出</th></tr></thead>
          <tbody>${by.map((n) => `
            <tr>
              <td>${n.node_id ? `#${n.node_id} <span class="muted">${esc((n.summary || '').slice(0, 28))}</span>` : '<span class="muted">任务级</span>'}</td>
              <td class="num">${n.calls}</td>
              <td><span class="bar-track" style="display:inline-block;width:140px;vertical-align:middle"><span class="bar-fill ${n.node_id ? '' : 'is-task'}" style="width:${Math.round((n.effective_input_tokens / max) * 100)}%"></span></span>
                <span class="mono muted">${fmtTok(n.effective_input_tokens)}</span></td>
              <td class="num">${fmtTok(n.output_tokens)}</td>
            </tr>`).join('')}</tbody>
        </table>
      </div>` : empty('还没有用量记录')}
    </div>`;
}

async function drawCoverage(id, body) {
  const ov = S.overview || await api(`/api/runs/${id}/overview`);
  const cov = ov.coverage || { total: 0, tested: 0, pct: 0, by_kind: [] };
  const C = 2 * Math.PI * 48;
  body.innerHTML = `
    <div class="card-body grid grid-2-even">
      <div class="donut">
        <svg viewBox="0 0 120 120" role="img" aria-label="覆盖度">
          <circle class="donut-track" cx="60" cy="60" r="48"></circle>
          <circle class="donut-fill" cx="60" cy="60" r="48"
            stroke-dasharray="${C}" stroke-dashoffset="${C * (1 - (cov.pct || 0) / 100)}"></circle>
        </svg>
        <div class="donut-caption">
          <b>${Math.round(cov.pct || 0)}%</b>
          <span>${cov.tested} / ${cov.total} 已测</span>
          <span>只认 fact 锚点，origin 不算</span>
        </div>
      </div>
      <div>
        ${(cov.by_kind || []).length ? `<table class="table">
          <thead><tr><th>类型</th><th class="num">总数</th><th class="num">已测</th></tr></thead>
          <tbody>${cov.by_kind.map((k) => `<tr><td>${k.kind}</td><td class="num">${k.total}</td><td class="num">${k.tested}</td></tr>`).join('')}</tbody>
        </table>` : empty('范围里还没有实体', '授权范围登记后，范围内的实体就是覆盖度分母。')}
      </div>
    </div>`;
}

/* ── 发现 / 资产 / 设置 ──────────────────────────────── */

async function renderFindings() {
  setPage('发现', '跨任务的已确认漏洞');
  const data = await api('/api/findings');
  S.findings = data.items || [];
  drawFindings();
  S.refresh = async () => { S.findings = (await api('/api/findings')).items || []; drawFindings(); };
}

function drawFindings() {
  $('view').innerHTML = `<div class="card">
    ${S.findings.length ? `<table class="table">
      <thead><tr><th>名称</th><th style="width:90px">等级</th><th>所属任务</th><th>目标</th><th>时间</th></tr></thead>
      <tbody>${S.findings.map((f) => `<tr class="is-clickable" data-run="${f.run_id}">
        <td>${esc(f.name || f.summary)}</td>
        <td>${f.severity ? `<span class="badge badge-failed">${esc(f.severity)}</span>` : '<span class="muted">—</span>'}</td>
        <td class="goal-cell"><span class="muted">#${f.run_id} ${esc(f.run_goal)}</span></td>
        <td class="mono muted">${esc(f.entity || '—')}</td>
        <td class="muted">${fmtDate(f.created_at)}</td>
      </tr>`).join('')}</tbody>
    </table>` : empty('还没有发现', 'worker 只有在<b>真实触发并拿到证据</b>时才会上报 finding；疑似一律记 fact。')}
  </div>`;
  document.querySelectorAll('[data-run]').forEach((row) => {
    row.addEventListener('click', () => { location.hash = `#/runs/${row.dataset.run}/overview`; });
  });
}

async function renderAssets() {
  setPage('资产', '跨任务共享的实体图');
  const data = await api('/api/assets');
  S.assets = data.items || [];
  drawAssets();
  S.refresh = async () => { S.assets = (await api('/api/assets')).items || []; drawAssets(); };
}

function drawAssets() {
  $('view').innerHTML = `<div class="card">
    ${S.assets.length ? `<table class="table">
      <thead><tr><th style="width:110px">类型</th><th>标识</th><th class="num">状态</th><th>标题</th><th class="num">任务</th><th>最近可见</th></tr></thead>
      <tbody>${S.assets.map((e) => {
        const ident = e.url || e.domain || (e.ip ? `${e.ip}${e.port ? ':' + e.port : ''}` : '') || e.service_name;
        return `<tr>
          <td><span class="chip">${esc(e.kind)}</span></td>
          <td class="mono">${esc(ident || '—')}</td>
          <td class="num">${e.status_code || '—'}</td>
          <td class="muted" style="max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(e.page_title || e.service_name || '')}</td>
          <td class="num">${(e.run_ids || []).length}</td>
          <td class="muted">${ago(e.last_seen)}</td>
        </tr>`;
      }).join('')}</tbody>
    </table>` : empty('还没有资产', 'worker 用 insert_assets 登记；去重由数据库唯一索引保证。')}
  </div>`;
}

async function renderSettings() {
  setPage('设置', '当前运行形态');
  const [health, stats] = await Promise.all([api('/health'), api('/api/stats')]);
  S.health = health;
  $('view').innerHTML = `
    <div class="grid grid-2-even">
      <div class="card">
        <div class="card-head"><h2 class="card-title">运行形态</h2></div>
        <div class="card-body stack" style="gap:10px">
          <div class="bar-row" style="grid-template-columns:120px 1fr"><span class="bar-label muted">版本</span><span class="mono">${esc(health.version)}</span></div>
          <div class="bar-row" style="grid-template-columns:120px 1fr"><span class="bar-label muted">数据库</span><span>${health.db ? '<span class="badge badge-done">可用</span>' : '<span class="badge badge-failed">不可用</span>'}</span></div>
          <div class="bar-row" style="grid-template-columns:120px 1fr"><span class="bar-label muted">模型</span><span>${health.llm_configured ? '已配置（真实 provider）' : 'fake / 未配置 —— 任务不会真正调用模型'}</span></div>
          <div class="bar-row" style="grid-template-columns:120px 1fr"><span class="bar-label muted">监听</span><span class="mono">127.0.0.1:8787（无鉴权，仅本机）</span></div>
        </div>
      </div>
      <div class="card">
        <div class="card-head"><h2 class="card-title">数据规模</h2></div>
        <div class="card-body">
          <table class="table">
            <tbody>
              <tr><td>任务</td><td class="num">${fmtInt(stats.runs)}</td></tr>
              <tr><td>意图</td><td class="num">${fmtInt(stats.intents)}</td></tr>
              <tr><td>事实</td><td class="num">${fmtInt(stats.facts)}</td></tr>
              <tr><td>发现</td><td class="num">${fmtInt(stats.findings)}</td></tr>
              <tr><td>资产</td><td class="num">${fmtInt(stats.entities)}</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
    <p class="page-foot">模型接入仍走环境变量（<code>.env.local</code>）：QINYUAN_LLM_PROVIDER / BASE_URL / API_KEY / MODEL。控制台只读展示，不写入凭据。</p>`;
}

/* ── 抽屉 ────────────────────────────────────────────── */

async function openDetail(a) {
  if (!a || !S.run) return;
  $('drawer').hidden = false;
  $('drawer-title').textContent = `#${a.id} · ${a.kind}${a.tool ? ' · ' + a.tool : ''}`;
  $('drawer-body').textContent = a.summary || '';
  try {
    const d = await api(`/api/runs/${S.run.id}/activity/${a.id}`);
    $('drawer-body').textContent = d.detail || d.summary || '(无正文)';
  } catch (e) {
    $('drawer-body').textContent = `${a.summary || ''}\n\n（详情读取失败：${e.message}）`;
  }
}

/* ── 启动 ────────────────────────────────────────────── */

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

(function boot() {
  applyTheme();
  $('btn-theme').addEventListener('click', () => {
    S.theme = S.theme === 'light' ? 'dark' : 'light';
    applyTheme();
  });
  $('btn-refresh').addEventListener('click', () => { if (S.refresh) S.refresh(); route(); });
  $('drawer-close').addEventListener('click', () => { $('drawer').hidden = true; });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') $('drawer').hidden = true; });
  $('view').addEventListener('scroll', () => {
    const v = $('view');
    S.stickBottom = v.scrollHeight - v.scrollTop - v.clientHeight < 120;
  });

  window.addEventListener('hashchange', route);
  pollGlobal();
  route();

  setInterval(() => {
    pollGlobal();
    if (S.refresh) S.refresh();
  }, 5000);
})();

