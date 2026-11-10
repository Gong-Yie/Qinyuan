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
  authed: false,
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
  init = init || {};
  const method = String(init.method || 'GET').toUpperCase();
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    // M9：cookie 会话的写请求必须带 CSRF 双提交头（服务端会比对 qy_csrf cookie）。
    init.headers = Object.assign({ 'X-CSRF-Token': csrfToken() }, init.headers || {});
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { raw: text }; }
  if (res.status === 401) {
    showLogin('登录已过期，请重新登录');
    throw new Error('未登录');
  }
  if (!res.ok) throw new Error((body && body.error) || `HTTP ${res.status}`);
  return body;
}

// ---------- M9：登录态 ----------

function csrfToken() {
  const m = document.cookie.match(/(?:^|;\s*)qy_csrf=([^;]*)/);
  return m ? decodeURIComponent(m[1]) : '';
}

function showLogin(msg) {
  S.authed = false;
  const mask = $('login');
  if (!mask) return;
  mask.hidden = false;
  if (msg) $('login-hint').textContent = msg;
  const err = $('login-err');
  err.hidden = true;
  setTimeout(() => $('login-token').focus(), 30);
}

function hideLogin() {
  const mask = $('login');
  if (mask) mask.hidden = true;
}

async function doLogin() {
  const token = $('login-token').value.trim();
  const err = $('login-err');
  if (!token) { err.textContent = '请输入令牌'; err.hidden = false; return; }
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      err.textContent = body.error || `登录失败（HTTP ${res.status}）`;
      err.hidden = false;
      return;
    }
    $('login-token').value = '';
    hideLogin();
    S.authed = true;
    $('auth-logout').hidden = false;
    await pollGlobal();
    await route();
  } catch (e) {
    err.textContent = '登录请求失败：' + e.message;
    err.hidden = false;
  }
}

async function doLogout() {
  try {
    await fetch('/api/logout', { method: 'POST', headers: { 'X-CSRF-Token': csrfToken() } });
  } catch { /* 忽略：本地刷新即可 */ }
  location.reload();
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
    $('badge-approvals').textContent = st.pending_approvals ? String(st.pending_approvals) : '';
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
  if (seg[0] === 'approvals') return { name: 'approvals' };
  if (seg[0] === 'campaigns' && seg[1]) return { name: 'campaign', campId: Number(seg[1]) };
  if (seg[0] === 'campaigns') return { name: 'campaigns' };
  if (seg[0] === 'ops') return { name: 'ops' };
  if (seg[0] === 'settings') return { name: 'settings' };
  return { name: 'dashboard' };
}

async function route() {
  if (!S.authed) { showLogin('需要登录后才能使用控制台'); return; }
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
      case 'approvals': await renderApprovals(); break;
      case 'campaigns': await renderCampaigns(); break;
      case 'campaign': await renderCampaign(); break;
      case 'ops': await renderOps(); break;
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
      <div class="card-head"><h2 class="card-title">新建任务</h2>
        <span class="muted">目标会按"最小范围"登记授权（URL / IP:端口 / 域名 / CIDR）；留空 = 只有目标文本</span></div>
      <div class="card-body">
        <div class="form-row">
          <input class="search" id="new-goal" placeholder="目标（必填）：如 拿下 /flag；或 确认登录页是否有 SQL 注入" style="min-width:420px" />
          <input class="search" id="new-target" placeholder="授权目标：http://靶机:8080/ 或 10.0.0.5 或 example.com" style="min-width:300px" />
          <input class="search" id="new-workers" type="number" min="1" max="8" value="2" title="worker 数" style="min-width:80px" />
          <input class="search" id="new-timeout" type="number" min="0" placeholder="超时秒(0=不限)" style="min-width:130px" />
          <input class="search" id="new-name" placeholder="任务名（可选）" style="min-width:150px" />
          <button class="btn btn-primary" id="new-create">创建并启动</button>
        </div>
      </div>
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
  const createBtn = $('new-create');
  if (createBtn) createBtn.addEventListener('click', createRun);

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
      ${r.can_start ? `<button class="btn btn-sm btn-primary" data-act="start" data-run="${r.id}">启动</button>` : ''}
      ${r.can_pause ? `<button class="btn btn-sm" data-act="pause" data-run="${r.id}">暂停</button>` : ''}
      ${r.can_resume ? `<button class="btn btn-sm" data-act="resume" data-run="${r.id}">恢复</button>` : ''}
      <button class="btn btn-sm" data-act="open" data-run="${r.id}">详情</button>
    </td>
  </tr>`;
}

async function control(id, action) {
  if (action === 'open') { location.hash = `#/runs/${id}`; return; }
  // M8：CI 导出 / 工单同步不走 /control（它们是集成动作，不是任务生命周期）。
  if (action === 'ci' || action === 'sync') {
    try {
      const r = await api(`/api/runs/${id}/${action === 'ci' ? 'ci-export' : 'sync-tickets'}`, { method: 'POST' });
      toast(action === 'ci'
        ? `CI 包已导出：${r.count} 个文件 → ${r.dir}`
        : `已回写 ${r.applied} 条工单状态`);
    } catch (e) { toast('操作失败：' + e.message); }
    return;
  }
  try {
    await api(`/api/runs/${id}/control`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action }),
    });
    toast({ pause: '已暂停', resume: '已恢复', start: '已启动' }[action] || '已处理');
    if (S.refresh) await S.refresh();
    await pollGlobal();
  } catch (e) { toast('操作失败：' + e.message); }
}

// 新建任务：创建 + 启动（两步都要，否则任务只是躺在 created 状态）。
async function createRun() {
  const goal = $('new-goal').value.trim();
  if (!goal) { toast('先写目标（goal 必填）'); return; }
  const body = {
    goal,
    target: $('new-target').value.trim(),
    name: $('new-name').value.trim(),
    worker_count: Number($('new-workers').value) || 2,
    timeout_seconds: Number($('new-timeout').value) || 0,
  };
  try {
    const run = await api('/api/runs', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    try {
      await api(`/api/runs/${run.id}/control`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"action":"start"}',
      });
      toast(`任务 #${run.id} 已创建并启动`);
    } catch (e) {
      toast(`任务 #${run.id} 已创建，但启动失败：${e.message}`);
    }
    location.hash = `#/runs/${run.id}`;
  } catch (e) {
    toast('创建失败：' + e.message);
  }
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

  const tabs = [
    ['overview', '概览'], ['sessions', '会话'], ['traffic', '流量'], ['graph', '链路'],
    ['plan', '计划'], ['timeline', '时间线'], ['cost', '成本'], ['coverage', '覆盖'], ['covgraph', '覆盖图'],
  ];
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
            ${runActions(run, id)}
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
        acts.innerHTML = runActions(r, id);
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
  if (tab === 'traffic') return drawTraffic(id, body);
  if (tab === 'graph') return drawGraph(id, body);
  if (tab === 'plan') return drawPlan(id, body);
  if (tab === 'timeline') return drawTimeline(id, body);
  if (tab === 'cost') return drawCost(id, body);
  if (tab === 'coverage') return drawCoverage(id, body);
  if (tab === 'covgraph') return drawCoverageGraph(id, body);
}

/* ── 链路图 / 覆盖图 ────────────────────────────────── */

/* ── 流量 ────────────────────────────────────────────── */

async function drawTraffic(id, body) {
  const q = S.trafficQ || '';
  const data = await api(`/api/runs/${id}/traffic?limit=50${q ? '&q=' + encodeURIComponent(q) : ''}`);
  const items = data.items || [];
  const sig = `${items.length}:${items[0] ? items[0].id : 0}:${q}`;
  if (S.trafficSig === sig && $('traffic-body')) return;
  S.trafficSig = sig;

  body.innerHTML = `
    <div class="card-body" style="padding:0">
      <div class="toolbar" style="padding:12px 16px 0">
        <input class="search" id="traffic-q" placeholder="正文子串（响应/请求体）…" value="${esc(q)}" />
        <button class="btn" id="traffic-search">搜索</button>
        <button class="btn btn-ghost" id="traffic-clear">清空</button>
        <span class="spacer"></span>
        <span class="muted">${items.length} 条 · 录制在 http_request 内完成（带 intent 归属）</span>
      </div>
      <div id="traffic-body">
        ${items.length ? `<table class="table">
          <thead><tr>
            <th class="num">#</th><th>方法</th><th>URL</th><th class="num">状态</th>
            <th class="num">长度</th><th class="num">耗时</th><th class="num">意图</th><th>时间</th>
          </tr></thead>
          <tbody>${items.map((t) => `<tr class="is-clickable" data-traffic="${t.id}">
            <td class="mono">${t.id}</td>
            <td class="mono">${esc(t.method)}</td>
            <td class="goal-cell"><span class="mono">${esc(t.url)}</span></td>
            <td class="num">${t.status || (t.error ? '错误' : '—')}</td>
            <td class="num">${fmtInt(t.resp_len)}</td>
            <td class="num">${t.duration_ms}ms</td>
            <td class="num">${t.intent_id ? '#' + t.intent_id : '—'}</td>
            <td class="muted">${fmtTime(t.ts)}</td>
          </tr>`).join('')}</tbody>
        </table>` : empty('还没有流量', 'worker 每次 http_request 都会在这里留一条（含请求/响应与 intent 归属）。')}
      </div>
    </div>`;

  const doSearch = () => {
    S.trafficQ = ($('traffic-q').value || '').trim();
    S.trafficSig = '';
    drawTraffic(id, body);
  };
  $('traffic-search').addEventListener('click', doSearch);
  $('traffic-q').addEventListener('keydown', (e) => { if (e.key === 'Enter') doSearch(); });
  $('traffic-clear').addEventListener('click', () => {
    S.trafficQ = '';
    S.trafficSig = '';
    drawTraffic(id, body);
  });
  document.querySelectorAll('[data-traffic]').forEach((row) => {
    row.addEventListener('click', async () => {
      try {
        const d = await api(`/api/runs/${id}/traffic/${row.dataset.traffic}`);
        openDrawerRaw(`流量 #${d.id} · ${d.method} ${d.url}`, JSON.stringify(d, null, 2));
      } catch (e) { toast('读取失败：' + e.message); }
    });
  });
}

async function drawGraph(id, body) {
  const data = await api(`/api/runs/${id}/graph`);
  const sig = `${data.nodes.length}:${data.edges.length}:${data.nodes.map((n) => n.id + n.state).join(',').length}`;
  // 结构没变就别重画：轮询每 5s 一次，重画会把用户的缩放/平移冲掉。
  if (S.graphSig === sig && $('graph-host')) return;
  S.graphSig = sig;
  body.innerHTML = '<div class="card-body"><div class="graph-host" id="graph-host"></div></div>';
  const host = $('graph-host');
  QY_GRAPH.renderExploration(host, data, (n) => {
    openDrawerRaw(`#${n.id} · ${n.kind}`, JSON.stringify(n, null, 2));
  });
}

async function drawCoverageGraph(id, body) {
  const data = await api(`/api/runs/${id}/coverage-graph`);
  const sig = `${data.nodes.length}:${data.edges.length}`;
  if (S.covSig === sig && $('cov-host')) return;
  S.covSig = sig;
  body.innerHTML = '<div class="card-body"><div class="graph-host" id="cov-host"></div></div>';
  const host = $('cov-host');
  QY_GRAPH.renderCoverage(host, data, (n) => {
    openDrawerRaw(`${n.key} · ${n.kind}`, JSON.stringify(n, null, 2));
  });
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
    </div>
    <div class="card-body" style="padding-top:0">
      <p class="page-foot">分母 = 授权范围命中的实体；分子只认 <b>fact 锚点</b>（finding / 意图 / origin 都不算）。
        若范围是 <code>ip</code>、而事实锚在 <code>service</code>/<code>endpoint</code> 上，覆盖率会偏低 ——
        这是范围语义问题（见「覆盖图」里的范围外节点）。</p>
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
      <thead><tr><th>名称</th><th style="width:90px">等级</th><th>所属任务</th><th>目标</th><th>时间</th><th style="width:170px">复测</th></tr></thead>
      <tbody>${S.findings.map((f) => `<tr class="is-clickable" data-run="${f.run_id}">
        <td>${esc(f.name || f.summary)}</td>
        <td>${f.severity ? `<span class="badge badge-failed">${esc(f.severity)}</span>` : '<span class="muted">—</span>'}</td>
        <td class="goal-cell"><span class="muted">#${f.run_id} ${esc(f.run_goal)}</span></td>
        <td class="mono muted">${esc(f.entity || '—')}</td>
        <td class="muted">${fmtDate(f.created_at)}</td>
        <td><span class="retest-slot" data-slot="${f.run_id}:${f.id}"><span class="muted">—</span></span>
          <button class="btn btn-sm" data-retest="${f.id}" data-r="${f.run_id}">复测</button></td>
      </tr>`).join('')}</tbody>
    </table>` : empty('还没有发现', 'worker 只有在<b>真实触发并拿到证据</b>时才会上报 finding；疑似一律记 fact。')}
  </div>`;
  document.querySelectorAll('[data-run]').forEach((row) => {
    row.addEventListener('click', (ev) => {
      if (ev.target.closest('button')) return;
      location.hash = `#/runs/${row.dataset.run}/overview`;
    });
  });
  document.querySelectorAll('[data-retest]').forEach((btn) => {
    btn.addEventListener('click', () => retestFinding(Number(btn.dataset.r), Number(btn.dataset.retest), btn));
  });
}

// retestFinding 调复测并就地显示结论（verified / failed / unverifiable）。
async function retestFinding(runId, findingId, btn) {
  btn.disabled = true;
  btn.textContent = '复测中…';
  try {
    const res = await api(`/api/runs/${runId}/findings/${findingId}/retest`, { method: 'POST' });
    const first = (res.items || [])[0];
    const slot = document.querySelector(`[data-slot="${runId}:${findingId}"]`);
    if (slot && first) {
      const cls = first.status === 'verified' ? 'badge-done'
        : first.status === 'failed' ? 'badge-failed' : 'badge-paused';
      const label = first.status === 'verified' ? '已验证'
        : first.status === 'failed' ? '已失效' : '无法判定';
      slot.innerHTML = `<span class="badge ${cls}">${label}</span>`;
      slot.title = first.observed || '';
    }
    toast(first ? `${first.status}：${first.observed || ''}`.slice(0, 90) : '复测完成');
  } catch (e) {
    toast('复测失败：' + e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = '复测';
  }
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

/* ── 审批 ────────────────────────────────────────────── */

async function renderApprovals() {
  setPage('审批', '规则命中后挂起的人工确认');
  const [pending, all, rules] = await Promise.all([
    api('/api/approvals?status=pending'),
    api('/api/approvals?limit=50'),
    api('/api/intercept/rules'),
  ]);
  const history = (all.items || []).filter((x) => x.status !== 'pending');

  $('view').innerHTML = `
    <div class="stack">
      <div class="card">
        <div class="card-head">
          <h2 class="card-title">待处理<small>${(pending.items || []).length}</small></h2>
          <span class="muted">工具调用会一直阻塞，直到你决定或超时</span>
        </div>
        ${(pending.items || []).length ? `<div class="toolbar" style="padding:12px 16px 0">
          <label class="muted" style="display:flex;gap:6px;align-items:center">
            <input type="checkbox" id="ap-all" /> 全选
          </label>
          <button class="btn btn-primary btn-sm" id="ap-approve">批量批准</button>
          <button class="btn btn-danger btn-sm" id="ap-deny">批量拒绝</button>
          <span class="muted">冲突项（已被别人处理 / 已超时）不会被覆盖</span>
        </div>` : ''}
        ${(pending.items || []).length ? (pending.items || []).map((p) => `
          <div class="row-item">
            <input type="checkbox" class="ap-check" value="${p.id}" />
            <span class="state-dot running"></span>
            <div class="row-main">
              <div class="row-title"><b class="mono">#${p.id}</b> · <span class="mono">${esc(p.tool)}</span>
                <span class="muted">（任务 #${p.run_id}${p.intent_id ? ' · 意图 #' + p.intent_id : ''}）</span></div>
              <div class="row-meta"><span>${esc(p.reason)}</span><span>${fmtDate(p.created_at)}</span></div>
              <pre class="approval-code">${esc(p.tool_input || '{}')}</pre>
            </div>
            <div class="row-side">
              <button class="btn btn-primary btn-sm" data-approve="${p.id}">批准</button>
              <button class="btn btn-danger btn-sm" data-deny="${p.id}">拒绝</button>
            </div>
          </div>`).join('') : empty('没有待处理的审批', '规则命中 <b>ask</b> 时工具调用会挂在这里等人工确认。')}
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">审批记录</h2></div>
        ${history.length ? `<table class="table">
          <thead><tr><th>#</th><th>工具</th><th>结果</th><th>处理人</th><th>时间</th><th>原因</th></tr></thead>
          <tbody>${history.map((p) => `<tr>
            <td class="mono">#${p.id}</td>
            <td class="mono">${esc(p.tool)}</td>
            <td>${p.status === 'allowed' ? '<span class="badge badge-done">已批准</span>'
              : p.status === 'denied' ? '<span class="badge badge-failed">已拒绝</span>'
              : '<span class="badge badge-paused">超时</span>'}</td>
            <td class="muted">${esc(p.decided_by || '—')}</td>
            <td class="muted">${p.decided_at ? fmtDate(p.decided_at) : '—'}</td>
            <td class="muted" style="max-width:420px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(p.reason)}</td>
          </tr>`).join('')}</tbody>
        </table>` : empty('还没有审批记录')}
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">审批规则<small>${(rules.items || []).length}</small></h2>
          <span class="muted">deny 永远优先；其余按 priority 降序取第一条</span></div>
        <table class="table">
          <thead><tr><th class="num">优先级</th><th>工具</th><th>参数正则</th><th>决定</th><th>说明</th></tr></thead>
          <tbody>${(rules.items || []).map((r) => `<tr>
            <td class="num">${r.priority}</td>
            <td class="mono">${esc(r.match_tool || '（任意）')}</td>
            <td class="mono muted">${esc(r.match_regex || '（任意）')}</td>
            <td>${r.decision === 'allow' ? '<span class="badge badge-done">放行</span>'
              : r.decision === 'deny' ? '<span class="badge badge-failed">禁止</span>'
              : '<span class="badge badge-paused">审批</span>'}</td>
            <td class="muted">${esc(r.note)}</td>
          </tr>`).join('')}</tbody>
        </table>
      </div>
    </div>`;

  document.querySelectorAll('[data-approve]').forEach((b) => {
    b.addEventListener('click', () => decideApproval(Number(b.dataset.approve), 'allowed', ''));
  });
  document.querySelectorAll('[data-deny]').forEach((b) => {
    b.addEventListener('click', () => {
      const reason = window.prompt('拒绝原因（会写进记录，并作为工具结果交回模型）：', '不符合授权范围');
      if (reason === null) return;
      decideApproval(Number(b.dataset.deny), 'denied', reason);
    });
  });

  // P3-4：批量决定（逐条 CAS，冲突项不覆盖）
  const allBox = $('ap-all');
  if (allBox) {
    allBox.addEventListener('change', () => {
      document.querySelectorAll('.ap-check').forEach((c) => { c.checked = allBox.checked; });
    });
  }
  const batch = async (decision) => {
    const ids = [...document.querySelectorAll('.ap-check')].filter((c) => c.checked).map((c) => Number(c.value));
    if (!ids.length) { toast('先勾选要处理的审批'); return; }
    if (decision === 'denied' && !window.confirm(`拒绝 ${ids.length} 条？拒绝原因会写进每条记录。`)) return;
    try {
      const r = await api('/api/approvals/batch-decide', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ids, decision, reason: decision === 'denied' ? '批量拒绝' : '' }),
      });
      toast(`成功 ${r.ok} 条${r.conflict && r.conflict.length ? `，冲突 ${r.conflict.length} 条（#${r.conflict.join(', #')}）` : ''}`);
      await pollGlobal();
      await renderApprovals();
    } catch (e) { toast('批量处理失败：' + e.message); }
  };
  if ($('ap-approve')) $('ap-approve').addEventListener('click', () => batch('allowed'));
  if ($('ap-deny')) $('ap-deny').addEventListener('click', () => batch('denied'));

  S.refresh = async () => { if (S.route && S.route.name === 'approvals') await renderApprovals(); };
}

async function decideApproval(id, decision, reason) {
  try {
    await api(`/api/approvals/${id}/decide`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ decision, reason }),
    });
    toast(decision === 'allowed' ? `已批准 #${id}` : `已拒绝 #${id}`);
    await pollGlobal();
    await renderApprovals();
  } catch (e) {
    toast('处理失败：' + e.message);
  }
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
  openDrawerRaw(`#${a.id} · ${a.kind}${a.tool ? ' · ' + a.tool : ''}`, a.summary || '');
  try {
    const d = await api(`/api/runs/${S.run.id}/activity/${a.id}`);
    $('drawer-body').textContent = d.detail || d.summary || '(无正文)';
  } catch (e) {
    $('drawer-body').textContent = `${a.summary || ''}\n\n（详情读取失败：${e.message}）`;
  }
}

function openDrawerRaw(title, text) {
  $('drawer').hidden = false;
  $('drawer-title').textContent = title;
  $('drawer-body').textContent = text;
}

/* ── M8：时间线 / 战役 / 运维 ───────────────────────── */

function runActions(run, id) {
  return `${run.can_start ? '<button class="btn btn-primary" data-act="start">启动</button>' : ''}
    ${run.can_pause ? '<button class="btn" data-act="pause">暂停</button>' : ''}
    ${run.can_resume ? '<button class="btn" data-act="resume">恢复</button>' : ''}
    <a class="btn btn-ghost" target="_blank" rel="noopener" href="/api/runs/${id}/report">报告 MD</a>
    <a class="btn btn-ghost" target="_blank" rel="noopener" href="/api/runs/${id}/report?format=html">HTML</a>
    <a class="btn btn-ghost" href="/api/runs/${id}/report?format=zip&download=1">报告包</a>
    <button class="btn btn-ghost" data-act="ci">CI 导出</button>
    <button class="btn btn-ghost" data-act="sync">同步工单</button>
    <a class="btn btn-ghost" href="#/runs">返回列表</a>`;
}

const CAMP_LABEL = { active: '进行中', paused: '已暂停', done: '已完成', archived: '已归档' };
const campBadge = (st) => {
  const k = st === 'active' ? 'running' : st === 'paused' ? 'paused' : st === 'done' ? 'done' : 'idle';
  return `<span class="badge badge-${k}">${CAMP_LABEL[st] || st || '未知'}</span>`;
};

/* 时间线：span 树 + 分位统计（P2-5） */
async function drawTimeline(id, body) {
  const minMs = S.tlMinMs || 0;
  const data = await api(`/api/runs/${id}/timeline?stats=1&limit=2000${minMs ? '&min_ms=' + minMs : ''}`);
  const spans = data.spans || [];
  const stats = data.by_name || [];
  let maxMs = 1;
  const walk = (ns) => (ns || []).forEach((n) => {
    maxMs = Math.max(maxMs, n.duration_ms || 0);
    walk(n.children);
  });
  walk(spans);

  const row = (n, depth) => {
    const w = Math.max(2, Math.round(((n.duration_ms || 0) / maxMs) * 100));
    const attrs = esc(JSON.stringify(n.attrs || {}, null, 1));
    const mark = n.children && n.children.length ? '▾' : '·';
    return `<div class="tl-row" data-attrs="${attrs}" title="点开看属性">
      <span class="tl-name" style="padding-left:${14 + depth * 18}px">${mark} ${esc(n.span)}</span>
      <span class="tl-bar-wrap"><span class="tl-bar ${n.error ? 'tl-bar-err' : ''}" style="display:block;width:${w}%"></span></span>
      <span class="tl-dur ${n.error ? 'tl-err' : ''}">${n.duration_ms}ms</span>
    </div>${(n.children || []).map((c) => row(c, depth + 1)).join('')}`;
  };

  body.innerHTML = `
    <div class="card-body" style="padding:0">
      <div class="toolbar" style="padding:12px 16px 0">
        <input class="search" id="tl-min" type="number" min="0" placeholder="只显示 ≥ N ms" value="${minMs || ''}" />
        <button class="btn" id="tl-apply">过滤</button>
        <span class="muted">trace <code>${esc(data.trace || '')}</code> · ${data.count || 0} 个 span</span>
      </div>
      ${stats.length ? `<div class="card-body" style="padding:8px 16px 0">
        <table class="table">
          <thead><tr><th>span</th><th class="num">次数</th><th class="num">p50</th><th class="num">p95</th><th class="num">p99</th><th class="num">最长</th><th class="num">错误</th></tr></thead>
          <tbody>${stats.map((s) => `<tr>
            <td class="mono">${esc(s.span)}</td>
            <td class="num">${s.count}</td>
            <td class="num">${Math.round(s.p50_ms)}ms</td>
            <td class="num">${Math.round(s.p95_ms)}ms</td>
            <td class="num">${Math.round(s.p99_ms)}ms</td>
            <td class="num">${s.max_ms}ms</td>
            <td class="num">${s.errors || 0}</td>
          </tr>`).join('')}</tbody>
        </table></div>` : ''}
      <div class="tl-list">${spans.length ? spans.map((n) => row(n, 0)).join('')
        : empty('还没有 span', '跑一轮规划/执行之后，这里会出现"轮 → 意图 → LLM/工具"的层级。')}</div>
    </div>`;

  $('tl-apply').addEventListener('click', () => {
    S.tlMinMs = Number($('tl-min').value) || 0;
    drawTab(id, 'timeline');
  });
  body.querySelectorAll('.tl-row').forEach((r) => {
    r.addEventListener('click', () => openDrawerRaw(r.querySelector('.tl-name').textContent.trim(), r.dataset.attrs));
  });
}

/* 战役列表（P3-1） */
async function renderCampaigns() {
  setPage('战役', '跨任务的覆盖度、进度与重复投入');
  const data = await api('/api/campaigns');
  const items = data.items || [];
  $('view').innerHTML = `
    <div class="stack">
      <div class="card">
        <div class="card-head"><h2 class="card-title">新建战役</h2>
          <span class="muted">战役是 run 的父容器：用来回答"整体打到哪"</span></div>
        <div class="card-body">
          <div class="form-row">
            <input class="search" id="camp-name" placeholder="战役名（如：客户 A 外网）" />
            <input class="search" id="camp-goal" placeholder="战役目标（可被证据判定）" style="min-width:320px" />
            <button class="btn btn-primary" id="camp-create">创建</button>
          </div>
        </div>
      </div>
      <div class="card">
        <div class="card-head"><h2 class="card-title">全部战役<small>${items.length}</small></h2></div>
        ${items.length ? `<table class="table">
          <thead><tr><th>#</th><th>名称</th><th>状态</th><th class="num">子任务</th><th>目标</th><th></th></tr></thead>
          <tbody>${items.map((c) => `<tr class="is-clickable" data-camp="${c.id}">
            <td class="mono">#${c.id}</td>
            <td><b>${esc(c.name)}</b></td>
            <td>${campBadge(c.status)}</td>
            <td class="num">${c.runs ?? 0}</td>
            <td class="goal-cell"><span>${esc(c.goal)}</span></td>
            <td><a class="btn btn-sm btn-ghost" href="#/campaigns/${c.id}">打开</a></td>
          </tr>`).join('')}</tbody></table>` : empty('还没有战役', '在上面建一个，再把子任务挂进来。')}
      </div>
    </div>`;

  $('camp-create').addEventListener('click', async () => {
    const name = $('camp-name').value.trim();
    const goal = $('camp-goal').value.trim();
    if (!name || !goal) { toast('战役名与目标都必填'); return; }
    try {
      const c = await api('/api/campaigns', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, goal }),
      });
      toast(`战役 #${c.id} 已创建`);
      location.hash = `#/campaigns/${c.id}`;
    } catch (e) { toast('创建失败：' + e.message); }
  });
  document.querySelectorAll('[data-camp]').forEach((tr) => {
    tr.addEventListener('click', (e) => {
      if (e.target.closest('a')) return;
      location.hash = `#/campaigns/${tr.dataset.camp}`;
    });
  });
}

/* 战役详情：三个聚合 + 子 run + 范围（P3-1） */
async function renderCampaign() {
  const id = S.route.campId;
  const sum = await api(`/api/campaigns/${id}`);
  setPage(`战役 #${id}`, sum.goal);
  const cov = sum.coverage || { total: 0, tested: 0, pct: 0 };
  const goals = sum.goals || { total: 0, met: 0 };
  const runs = sum.run_list || [];
  const dupes = sum.duplicates || [];
  const scope = sum.scope || [];

  $('view').innerHTML = `
    <div class="stack">
      <div class="card">
        <div class="detail-head">
          <h2>${esc(sum.name)} ${campBadge(sum.status)}</h2>
          <div class="detail-meta">
            <span>目标 <b>${esc(sum.goal)}</b></span>
            <span>子任务 <b>${runs.length}</b></span>
            <span>创建 <b>${fmtDate(sum.created_at)}</b></span>
          </div>
          <div class="detail-actions">
            <button class="btn" data-cact="pause">暂停全部</button>
            <button class="btn" data-cact="resume">恢复全部</button>
            <button class="btn btn-ghost" data-cact="done">标记完成</button>
            <button class="btn btn-ghost" data-cact="archive">归档</button>
            <a class="btn btn-ghost" href="#/campaigns">返回战役</a>
          </div>
        </div>
      </div>

      <div class="grid grid-kpi">
        <div class="card kpi">
          <div class="kpi-label">总覆盖度</div>
          <div class="kpi-value">${Math.round(cov.pct || 0)}<small>%</small></div>
          <div class="kpi-sub"><b>${cov.tested}</b> / ${cov.total} 个范围内实体（只认事实锚点）</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">目标进度</div>
          <div class="kpi-value">${goals.met}<small>/${goals.total}</small></div>
          <div class="kpi-sub">所有子任务的目标状态聚合</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">重复投入</div>
          <div class="kpi-value">${dupes.length}<small>个实体</small></div>
          <div class="kpi-sub">被 ≥2 个子任务测过：该拆分或者该合并的信号</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">范围内实体</div>
          <div class="kpi-value">${cov.total}</div>
          <div class="kpi-sub">由战役范围决定（子 run 继承，只许减不许加）</div>
        </div>
      </div>

      <div class="grid grid-2">
        <div class="card">
          <div class="card-head"><h2 class="card-title">子任务<small>${runs.length}</small></h2>
            <span class="muted">挂一个新任务进来</span></div>
          <div class="card-body">
            <div class="form-row" style="margin-bottom:12px">
              <input class="search" id="att-run" type="number" min="1" placeholder="run id" style="min-width:110px" />
              <input class="search" id="att-role" placeholder="这一块负责什么（如 外网 / 横向）" />
              <button class="btn" id="att-btn">挂载</button>
            </div>
            ${runs.length ? `<table class="table">
              <thead><tr><th>#</th><th>负责</th><th>状态</th><th class="num">事实</th><th class="num">发现</th><th class="num">覆盖</th></tr></thead>
              <tbody>${runs.map((r) => `<tr class="is-clickable" data-gorun="${r.run_id}">
                <td class="mono">#${r.run_id}</td>
                <td>${esc(r.role || '—')}</td>
                <td>${statusBadge(r.status)}</td>
                <td class="num">${r.facts}</td>
                <td class="num">${r.findings}</td>
                <td class="num">${Math.round(r.coverage_pct || 0)}%</td>
              </tr>`).join('')}</tbody></table>` : empty('还没有子任务', '用上面的输入框把 run id 挂进来。')}
          </div>
        </div>

        <div class="card">
          <div class="card-head"><h2 class="card-title">战役范围<small>${scope.length}</small></h2></div>
          <div class="card-body">
            <div class="form-row" style="margin-bottom:12px">
              <select class="search" id="sc-kind" style="min-width:150px">
                <option value="root_domain">root_domain（含子域）</option>
                <option value="subdomain">subdomain（精确）</option>
                <option value="ip">ip</option>
                <option value="cidr">cidr</option>
              </select>
              <input class="search" id="sc-value" placeholder="example.com / 10.0.0.0/8" />
              <button class="btn" id="sc-btn">登记</button>
            </div>
            ${scope.length ? `<table class="table">
              <thead><tr><th>类型</th><th>值</th></tr></thead>
              <tbody>${scope.map((s) => `<tr>
                <td class="mono">${esc(s.kind)}</td>
                <td class="mono">${esc(s.domain || s.net || s.value || '')}</td>
              </tr>`).join('')}</tbody></table>` : empty('还没有战役范围', '没有范围时总覆盖度会一直是 0/0。')}
          </div>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">重复投入<small>${dupes.length}</small></h2>
          <span class="muted">同一实体被多个子任务测过 —— 信息在这里，不阻断执行</span></div>
        ${dupes.length ? `<table class="table">
          <thead><tr><th>实体</th><th>类型</th><th>测过它的任务</th></tr></thead>
          <tbody>${dupes.map((d) => `<tr>
            <td class="mono">#${d.entity_id} ${esc(d.label || '')}</td>
            <td>${esc(d.kind)}</td>
            <td class="mono">${(d.runs || []).map((r) => '#' + r).join(' · ')}</td>
          </tr>`).join('')}</tbody></table>` : empty('没有重复投入', '每个实体只被一个子任务测过。')}
      </div>
    </div>`;

  document.querySelectorAll('[data-cact]').forEach((b) => {
    b.addEventListener('click', async () => {
      try {
        const r = await api(`/api/campaigns/${id}/control`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action: b.dataset.cact }),
        });
        toast(`战役已${CAMP_LABEL[b.dataset.cact] || b.dataset.cact}${r.affected_runs != null ? '（影响 ' + r.affected_runs + ' 个子任务）' : ''}`);
        await renderCampaign();
      } catch (e) { toast('操作失败：' + e.message); }
    });
  });
  $('att-btn').addEventListener('click', async () => {
    const runID = Number($('att-run').value);
    if (!runID) { toast('填 run id'); return; }
    try {
      await api(`/api/campaigns/${id}/runs`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ run_id: runID, role: $('att-role').value.trim() }),
      });
      toast(`run #${runID} 已挂到战役 #${id}`);
      await renderCampaign();
    } catch (e) { toast('挂载失败：' + e.message); }
  });
  $('sc-btn').addEventListener('click', async () => {
    const kind = $('sc-kind').value;
    const value = $('sc-value').value.trim();
    if (!value) { toast('填范围值'); return; }
    const body = { kind };
    if (kind === 'cidr') body.net = value; else body.domain = value;
    try {
      await api(`/api/campaigns/${id}/scope`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      toast('范围已登记');
      await renderCampaign();
    } catch (e) { toast('登记失败：' + e.message); }
  });
  document.querySelectorAll('[data-gorun]').forEach((tr) => {
    tr.addEventListener('click', () => { location.hash = `#/runs/${tr.dataset.gorun}`; });
  });
  S.refresh = async () => { if (S.route && S.route.name === 'campaign') await renderCampaign(); };
}

/* 运维：outbox / 连接器 / 审批策略（P2-3 / P3-3 / P3-4） */
async function renderOps() {
  setPage('运维', '事件投递、外部集成与预授权策略');
  const [ob, conn, pols, grants] = await Promise.all([
    api('/api/outbox?status=dead&limit=50'),
    api('/api/connectors/deliveries?limit=50'),
    api('/api/approval-policies'),
    api('/api/approval-grants?limit=30'),
  ]);
  const dead = ob.items || [];
  const deliveries = conn.items || [];
  const enabled = conn.enabled || [];
  const policies = pols.items || [];

  $('view').innerHTML = `
    <div class="stack">
      <div class="grid grid-kpi">
        <div class="card kpi">
          <div class="kpi-label">outbox 积压</div>
          <div class="kpi-value">${ob.pending ?? 0}</div>
          <div class="kpi-sub">待投递事件（提交了就一定会送）</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">死信</div>
          <div class="kpi-value">${ob.dead ?? 0}</div>
          <div class="kpi-sub">停止自动重试，可人工重放</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">已启用连接器</div>
          <div class="kpi-value">${enabled.length}</div>
          <div class="kpi-sub">${enabled.length ? enabled.map((x) => esc(x)).join(' · ') : '未配置（QINYUAN_CONNECTOR_*）'}</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">预授权策略</div>
          <div class="kpi-value">${policies.length}</div>
          <div class="kpi-sub">每条放行都有审计，可一键撤销</div>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">死信<small>${dead.length}</small></h2>
          <span class="muted">修好下游后点"重放"</span></div>
        ${dead.length ? `<table class="table">
          <thead><tr><th>#</th><th>topic</th><th class="num">尝试</th><th>最后错误</th><th></th></tr></thead>
          <tbody>${dead.map((e) => `<tr>
            <td class="mono">#${e.id}</td>
            <td class="mono">${esc(e.topic)}</td>
            <td class="num">${e.attempts}</td>
            <td class="muted" style="max-width:520px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(e.last_error || '')}</td>
            <td><button class="btn btn-sm" data-replay="${e.id}">重放</button></td>
          </tr>`).join('')}</tbody></table>` : empty('没有死信', '投递失败超过上限的事件会出现在这里。')}
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">投递台账<small>${deliveries.length}</small></h2>
          <span class="muted">幂等键 = connector + event_key；推 5 次只有一个外部对象</span></div>
        ${deliveries.length ? `<table class="table">
          <thead><tr><th>连接器</th><th>事件</th><th>外部 id</th><th>状态</th><th>错误</th></tr></thead>
          <tbody>${deliveries.map((d) => `<tr>
            <td class="mono">${esc(d.connector)}</td>
            <td class="mono">${esc(d.event_key)}</td>
            <td class="mono">${esc(d.external_id || '—')}</td>
            <td>${esc(d.status || '')}</td>
            <td class="muted">${esc(d.last_error || '')}</td>
          </tr>`).join('')}</tbody></table>` : empty('还没有投递记录', '配置连接器后，finding 入库会自动推给外部系统。')}
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">审批策略<small>${policies.length}</small></h2>
          <span class="muted">预授权（allow）必须带限速与到期；deny 永远优先</span></div>
        <div class="card-body">
          <div class="form-row" style="margin-bottom:12px">
            <input class="search" id="pol-tool" placeholder="工具名（空=任意）" style="min-width:150px" />
            <select class="search" id="pol-decision" style="min-width:120px">
              <option value="allow">allow 预授权</option>
              <option value="ask">ask 要人工</option>
              <option value="deny">deny 禁止</option>
            </select>
            <select class="search" id="pol-scope" style="min-width:140px">
              <option value="any">范围：任意</option>
              <option value="same_target">范围：同目标</option>
              <option value="in_scope">范围：授权内</option>
            </select>
            <input class="search" id="pol-rate" type="number" min="1" placeholder="每小时上限" style="min-width:120px" />
            <input class="search" id="pol-hours" type="number" min="1" max="8" placeholder="有效小时(<=8)" style="min-width:120px" />
            <button class="btn btn-primary" id="pol-create">新增</button>
          </div>
          ${policies.length ? `<table class="table">
            <thead><tr><th class="num">优先级</th><th>工具</th><th>范围</th><th>决定</th><th class="num">限速</th><th>到期</th><th></th></tr></thead>
            <tbody>${policies.map((p) => `<tr>
              <td class="num">${p.priority}</td>
              <td class="mono">${esc(p.match_tool || '（任意）')}</td>
              <td>${esc(p.match_scope)}</td>
              <td>${p.decision === 'allow' ? '<span class="badge badge-done">放行</span>'
                : p.decision === 'deny' ? '<span class="badge badge-failed">禁止</span>'
                : '<span class="badge badge-paused">审批</span>'}</td>
              <td class="num">${p.max_per_hour || '—'}</td>
              <td class="muted">${p.expires_at ? fmtDate(p.expires_at) : '—'}</td>
              <td><button class="btn btn-sm btn-danger" data-unpol="${p.id}">撤销</button></td>
            </tr>`).join('')}</tbody></table>` : empty('还没有策略', '没有策略时行为与升级前一致（只看规则表）。')}
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">放行审计<small>${(grants.items || []).length}</small></h2>
          <span class="muted">每一条"没问人就放过"的调用都在这里</span></div>
        ${(grants.items || []).length ? `<table class="table">
          <thead><tr><th>#</th><th>策略</th><th>任务</th><th>工具</th><th>目标</th><th>时间</th></tr></thead>
          <tbody>${(grants.items || []).map((g) => `<tr>
            <td class="mono">#${g.id}</td>
            <td class="mono">#${g.policy_id}</td>
            <td class="mono">#${g.run_id}</td>
            <td class="mono">${esc(g.tool)}</td>
            <td class="mono">${esc(g.target || '—')}</td>
            <td class="muted">${fmtDate(g.created_at)}</td>
          </tr>`).join('')}</tbody></table>` : empty('还没有预授权放行', '预授权生效后，每次放行都会留一条。')}
      </div>
    </div>`;

  document.querySelectorAll('[data-replay]').forEach((b) => {
    b.addEventListener('click', async () => {
      try {
        await api(`/api/outbox/${b.dataset.replay}/replay`, { method: 'POST' });
        toast(`事件 #${b.dataset.replay} 已重放`);
        await renderOps();
      } catch (e) { toast('重放失败：' + e.message); }
    });
  });
  $('pol-create').addEventListener('click', async () => {
    const decision = $('pol-decision').value;
    const body = {
      match_tool: $('pol-tool').value.trim(),
      decision,
      match_scope: $('pol-scope').value,
    };
    const rate = Number($('pol-rate').value) || 0;
    const hours = Number($('pol-hours').value) || 0;
    if (decision === 'allow') {
      if (!rate || !hours) { toast('预授权必须给限速与有效小时（否则等于无限放行）'); return; }
      body.max_per_hour = rate;
      body.expires_at = Math.floor(Date.now() / 1000) + hours * 3600;
    }
    try {
      await api('/api/approval-policies', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      toast('策略已新增');
      await renderOps();
    } catch (e) { toast('新增失败：' + e.message); }
  });
  document.querySelectorAll('[data-unpol]').forEach((b) => {
    b.addEventListener('click', async () => {
      if (!window.confirm('撤销后立即失效（正在等待的同类请求仍要人处理）。确认撤销？')) return;
      try {
        await api(`/api/approval-policies/${b.dataset.unpol}`, { method: 'DELETE' });
        toast('策略已撤销');
        await renderOps();
      } catch (e) { toast('撤销失败：' + e.message); }
    });
  });
  S.refresh = async () => { if (S.route && S.route.name === 'ops') await renderOps(); };
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

  // M9：登录 / 登出
  $('login-btn').addEventListener('click', doLogin);
  $('login-token').addEventListener('keydown', (e) => { if (e.key === 'Enter') doLogin(); });
  $('auth-logout').addEventListener('click', doLogout);

  window.addEventListener('hashchange', route);

  start();

  setInterval(() => {
    if (!S.authed) return;
    pollGlobal();
    if (S.refresh) S.refresh();
  }, 5000);
})();

// start 先确认身份：未认证只显示登录卡片，不去拉任何业务数据。
async function start() {
  try {
    const res = await fetch('/api/me');
    if (res.status === 401) {
      showLogin('需要登录后才能使用控制台');
      return;
    }
    const me = await res.json().catch(() => ({}));
    S.authed = true;
    // 服务端没启用鉴权（回环开发态）时，不显示"退出"按钮。
    $('auth-logout').hidden = !me.auth;
    hideLogin();
  } catch (e) {
    showLogin('无法连接后端：' + e.message);
    return;
  }
  await pollGlobal();
  await route();
}

