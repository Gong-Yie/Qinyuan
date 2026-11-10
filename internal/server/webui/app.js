/* 钦原控制台 · 前端逻辑
 *
 * 三条纪律（来自 实现规格/06 §6）：
 *  1. 历史 ∪ 实时按 id 去重合并，绝不把广播当数据源；
 *  2. 断线由 EventSource 自动重连（服务端用 Last-Event-ID 从 DB 补齐缺口）；
 *  3. 轮询只用于"态势"（overview / tokens / steps），活动流一律走 SSE。
 */

const state = {
  runs: [],
  runId: 0,
  overview: null,
  tokens: null,
  steps: [],
  activities: [],      // 已渲染的顺序（按 id 升序）
  seen: new Set(),     // 去重
  toolCards: new Map(),// tool_use_id -> {card, resultLine}
  lastId: 0,
  filter: 'all',
  es: null,
  connected: false,
  stickBottom: true,
};

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};
const fmtInt = (n) => (n == null ? '–' : Number(n).toLocaleString('en-US'));
const fmtTok = (n) => (n == null ? '–' : n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n));
const fmtTime = (unix) => {
  const d = new Date(unix * 1000);
  return [d.getHours(), d.getMinutes(), d.getSeconds()].map((v) => String(v).padStart(2, '0')).join(':');
};

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
  toast._t = setTimeout(() => { t.hidden = true; }, 2600);
}

/* ── 任务列表与选择 ─────────────────────────────────── */

async function loadRuns() {
  const data = await api('/api/runs');
  state.runs = data.items || [];
  const sel = $('run-select');
  sel.innerHTML = '';
  if (!state.runs.length) {
    sel.appendChild(el('option', null, '还没有任务'));
    setPill('未选择', 'idle');
    return;
  }
  for (const r of state.runs) {
    const o = el('option', null, `#${r.id} · ${r.status || '?'} · ${(r.goal || '').slice(0, 28)}`);
    o.value = String(r.id);
    sel.appendChild(o);
  }
  const wanted = Number(new URLSearchParams(location.search).get('run')) || state.runId || state.runs[0].id;
  if (state.runs.some((r) => r.id === wanted)) {
    sel.value = String(wanted);
    await selectRun(wanted);
  }
}

async function selectRun(id) {
  if (!id) return;
  state.runId = id;
  state.activities = [];
  state.seen.clear();
  state.toolCards.clear();
  state.lastId = 0;
  $('tape').innerHTML = '';
  $('drawer').hidden = true;
  history.replaceState(null, '', `?run=${id}`);
  document.querySelector('.rail-left').scrollTop = 0;
  if (state.es) { state.es.close(); state.es = null; }
  await Promise.all([loadOverview(), loadSteps(), loadTokens(), loadHistory()]);
  connectStream();
}

/* ── 态势 / 计划 / 成本 ─────────────────────────────── */

async function loadOverview() {
  if (!state.runId) return;
  try {
    state.overview = await api(`/api/runs/${state.runId}/overview`);
    renderRibbon();
    renderGoals();
    renderIntents();
    renderScopes();
    renderFacts();
    renderDonut();
    renderStatus();
  } catch (e) { console.warn('overview', e); }
}

async function loadSteps() {
  if (!state.runId) return;
  try {
    const data = await api(`/api/runs/${state.runId}/nodes?kind=step&limit=200`);
    state.steps = (data.items || []).slice().sort((a, b) => a.id - b.id);
    renderSteps();
  } catch (e) { console.warn('steps', e); }
}

async function loadTokens() {
  if (!state.runId) return;
  try {
    state.tokens = await api(`/api/runs/${state.runId}/tokens`);
    renderRibbon();
    renderCost();
    renderSpend();
  } catch (e) { console.warn('tokens', e); }
}

/* ── 渲染：指标带 ───────────────────────────────────── */

function renderRibbon() {
  const ov = state.overview, tk = state.tokens;
  const goals = (ov && ov.goals) || [];
  const met = goals.filter((g) => g.state === 'met').length;
  const abandoned = goals.filter((g) => g.state === 'abandoned').length;
  $('m-goals').textContent = String(met);
  $('m-goals-total').textContent = `/${Math.max(goals.length - abandoned, met)}`;
  $('m-eff').textContent = fmtTok(tk && tk.effective_input_tokens);
  $('m-cache').textContent = tk && tk.cache_hit_pct != null ? String(Math.round(tk.cache_hit_pct)) : '–';
  $('m-facts').textContent = ov ? fmtInt(ov.facts_total) : '–';
  $('m-findings').textContent = ov ? `/${fmtInt(ov.findings_total)}` : '/–';
  const cov = (ov && ov.coverage) || null;
  $('m-cov').textContent = cov && cov.pct != null ? String(Math.round(cov.pct)) : '–';
}

function renderStatus() {
  const ov = state.overview;
  const run = state.runs.find((r) => r.id === state.runId);
  // overview 是每 5s 拉的（新鲜）；列表里的 status 可能还是页面加载时的快照。
  const status = (ov && ov.status) || (run && run.status) || 'idle';
  const map = {
    running: '运行中', paused: '已暂停', done: '已完成',
    failed: '失败', timeout: '超时', idle: '空闲（等待）', created: '已创建',
  };
  setPill(map[status] || status, status);
  $('btn-pause').disabled = status !== 'running';
  $('btn-resume').disabled = status !== 'paused';

  // select 里的状态标签也跟着更新，避免"下拉写着 idle、胶囊写着运行中"。
  if (run) {
    const opt = [...$('run-select').options].find((o) => Number(o.value) === state.runId);
    if (opt) opt.textContent = `#${run.id} · ${status} · ${(run.goal || '').slice(0, 28)}`;
  }
}

function setPill(text, status) {
  const p = $('run-status');
  p.className = 'pill ' + (status === 'running' ? 'pill-running'
    : status === 'paused' ? 'pill-paused'
    : status === 'done' ? 'pill-done'
    : status === 'failed' || status === 'timeout' ? 'pill-fail' : 'pill-idle');
  p.querySelector('.pill-text').textContent = text;
}

/* ── 渲染：左栏 ─────────────────────────────────────── */

function renderGoals() {
  const goals = ((state.overview && state.overview.goals) || []).filter((g) => g.state !== 'abandoned');
  const box = $('goals');
  box.innerHTML = '';
  $('goal-count').textContent = String(goals.length);
  $('goals-empty').hidden = goals.length > 0;
  for (const g of goals) {
    const li = el('li', `goal is-${g.state}`);
    li.appendChild(el('span', 'dot'));
    const wrap = el('div');
    const text = el('span', 'goal-text', g.summary || '(无标题)');
    wrap.appendChild(text);
    wrap.appendChild(el('span', 'goal-id', `#${g.id}`));
    li.appendChild(wrap);
    box.appendChild(li);
  }
}

function renderSteps() {
  const box = $('steps');
  box.innerHTML = '';
  const steps = state.steps;
  $('plan-count').textContent = String(steps.length);
  $('steps-empty').hidden = steps.length > 0;
  const done = steps.filter((s) => s.state === 'done').length;
  const bar = $('plan-bar');
  bar.hidden = steps.length === 0;
  $('plan-bar-fill').style.width = steps.length ? `${(done / steps.length) * 100}%` : '0';
  for (const s of steps) {
    const payload = s.payload || {};
    const li = el('li', `step is-${s.state}`);
    li.appendChild(el('span', 'step-state', s.state));
    const body = el('div');
    body.appendChild(el('span', 'step-text', payload.text || s.summary || `步骤 #${s.id}`));
    if (payload.expected_evidence) {
      body.appendChild(el('span', 'step-expected', payload.expected_evidence));
    }
    li.appendChild(body);
    box.appendChild(li);
  }
}

function renderIntents() {
  const ov = state.overview;
  const intents = []
    .concat((ov && ov.running_intents) || [])
    .concat((ov && ov.open_intents) || []);
  const box = $('intents');
  box.innerHTML = '';
  $('intent-count').textContent = String(intents.length);
  $('intents-empty').hidden = intents.length > 0;
  for (const it of intents) {
    const li = el('li', `intent is-${it.state}`);
    li.appendChild(el('span', 'intent-id', `#${it.id}`));
    li.appendChild(el('span', 'intent-summary', it.summary));
    box.appendChild(li);
  }
}

function renderScopes() {
  const ov = state.overview;
  const box = $('scopes');
  box.innerHTML = '';
  const scopes = (ov && ov.scopes) || [];
  if (!scopes.length) {
    box.appendChild(el('li', null, '未登记范围（默认拒绝出网）'));
  } else {
    for (const s of scopes.slice(0, 8)) box.appendChild(el('li', null, s));
  }
}

function renderFacts() {
  const facts = ((state.overview && state.overview.recent_facts) || []).slice(-6).reverse();
  const box = $('facts');
  box.innerHTML = '';
  for (const f of facts) {
    const li = el('li');
    li.appendChild(el('span', 'fact-id', `#${f.id}`));
    li.appendChild(el('span', null, f.summary));
    box.appendChild(li);
  }
  if (!facts.length) box.appendChild(el('li', 'empty', '还没有事实。'));
}

function renderDonut() {
  const cov = (state.overview && state.overview.coverage) || null;
  const pct = cov && cov.pct != null ? cov.pct : 0;
  const C = 2 * Math.PI * 48;
  $('donut-fill').style.strokeDashoffset = String(C * (1 - pct / 100));
  $('cov-pct').textContent = cov && cov.pct != null ? `${Math.round(pct)}%` : '–';
  $('cov-detail').textContent = cov ? `${cov.tested} / ${cov.total} 已测` : '未登记范围';
}

function renderCost() {
  const tk = state.tokens;
  const rows = $('cost-rows');
  rows.innerHTML = '';
  const by = (tk && tk.by_node) || [];
  $('cost-empty').hidden = by.length > 0;
  $('cost-hint').hidden = by.length === 0;
  for (const n of by.slice(0, 8)) {
    const tr = el('tr');
    const name = n.node_id === 0 ? '任务级' : `#${n.node_id} ${(n.summary || '').slice(0, 14)}`;
    tr.appendChild(el('td', null, name));
    tr.appendChild(el('td', 'num', fmtTok(n.effective_input_tokens)));
    tr.appendChild(el('td', 'num', fmtTok(n.output_tokens)));
    rows.appendChild(tr);
  }
}

function renderSpend() {
  const tk = state.tokens;
  const box = $('spend-bars');
  box.innerHTML = '';
  const by = ((tk && tk.by_node) || []).slice().reverse(); // 时间顺序：任务级在前
  if (!by.length) return;
  const max = Math.max(...by.map((n) => Math.max(n.effective_input_tokens, 1)));
  for (const n of by) {
    const b = document.createElement('i');
    b.className = 'spend-bar' + (n.node_id === 0 ? ' is-task' : '');
    const h = Math.max(2, Math.round((n.effective_input_tokens / max) * 26));
    b.style.height = h + 'px';
    b.dataset.tip = `${n.node_id === 0 ? '任务级' : '#' + n.node_id} · ${fmtTok(n.effective_input_tokens)} tok`;
    box.appendChild(b);
  }
  $('spend-total').textContent = `${fmtTok(tk.effective_input_tokens)} tok · ${by.length} 节点`;
}

/* ── 活动带 ─────────────────────────────────────────── */

function categoryOf(a) {
  const w = a.worker || '';
  if (w === 'planner') return 'plan';
  if (w === 'system' || !w) return 'system';
  return 'work';
}

function kindClass(a) {
  if (a.kind === 'tool_use' || a.kind === 'tool_result') return 'kind-tool';
  if (a.kind === 'round') return 'kind-round';
  if (a.kind === 'result') return 'kind-result';
  if (a.kind === 'system') return 'kind-system';
  if (a.kind === 'thinking') return 'kind-thinking';
  if (a.kind === 'text') return 'kind-text';
  return 'kind-misc';
}

function appendActivity(a) {
  if (!a || state.seen.has(a.id)) return;
  if (a.kind === 'usage') return; // 流式累计帧，列表里是噪音
  state.seen.add(a.id);
  state.lastId = Math.max(state.lastId, a.id);
  state.activities.push(a);
  $('tape-empty').hidden = true;

  const cat = categoryOf(a);
  const tick = el('div', `tick ${kindClass(a)} tick-${a.kind}`);
  tick.dataset.id = String(a.id);
  tick.dataset.cat = cat;
  if (a.is_error) tick.classList.add('is-error');
  if (state.filter !== 'all' && state.filter !== cat) tick.hidden = true;

  tick.appendChild(el('span', 'tick-time', fmtTime(a.created_at)));
  tick.appendChild(el('span', 'tick-node', a.node_id ? `#${a.node_id}` : '—'));

  if (a.kind === 'tool_use' || a.kind === 'tool_result') {
    const key = a.tool_use_id || `a${a.id}`;
    const entry = state.toolCards.get(key);

    // 结果能配对到已渲染的请求卡片：把结果行追加进那张卡，不新起一行。
    if (a.kind === 'tool_result' && entry) {
      entry.card.classList.remove('is-pending');
      if (a.is_error) entry.card.classList.add('is-error');
      const line = el('div', 'tool-line result' + (a.is_error ? ' is-error' : ''));
      line.appendChild(el('span', 'tool-name', 'result'));
      line.appendChild(el('span', 'tool-payload', a.summary || ''));
      line.addEventListener('click', () => openDetail(a));
      entry.card.appendChild(line);
      maybeScroll();
      updateTapeCount();
      return;
    }

    tick.appendChild(el('span', 'tick-glyph', a.kind === 'tool_use' ? '→' : '←'));
    const body = el('div', 'tick-body');
    const card = el('div', 'tool-card' + (a.kind === 'tool_use' ? ' is-pending' : ' is-error'));
    const line = el('div', `tool-line ${a.kind === 'tool_use' ? 'request' : 'result is-error'}`);
    line.appendChild(el('span', 'tool-name', a.tool || (a.kind === 'tool_use' ? 'tool' : 'result')));
    line.appendChild(el('span', 'tool-payload', a.summary || ''));
    line.addEventListener('click', () => openDetail(a));
    card.appendChild(line);
    if (a.kind === 'tool_use') state.toolCards.set(key, { card, result: null });
    body.appendChild(card);
    tick.appendChild(body);
    $('tape').appendChild(tick);
    maybeScroll();
    updateTapeCount();
    return;
  }

  const glyph = a.kind === 'round' ? '◆' : a.kind === 'system' ? '!' : a.kind === 'result' ? '✓' : '·';
  tick.appendChild(el('span', 'tick-glyph', glyph));
  const body = el('div', 'tick-body');
  body.appendChild(el('div', 'tick-summary', a.summary || ''));
  tick.appendChild(body);
  if (a.kind === 'round') tick.classList.add('tick-round');
  if (a.kind === 'text') tick.classList.add('tick-text');
  if (a.kind === 'system') tick.classList.add('tick-system');
  if (a.kind === 'result') tick.classList.add('tick-result');
  if (a.kind === 'thinking') tick.classList.add('tick-thinking');
  tick.addEventListener('click', () => openDetail(a));
  $('tape').appendChild(tick);
  maybeScroll();
  updateTapeCount();
}

function updateTapeCount() {
  const n = state.activities.length;
  const conn = state.connected ? '流已连接' : '流重连中';
  $('tape-count').textContent = `${n} 条 · ${conn}`;
}

function maybeScroll() {
  const tape = $('tape');
  const near = tape.scrollHeight - tape.scrollTop - tape.clientHeight < 80;
  $('to-bottom').hidden = near;
  if (near || state.stickBottom) tape.scrollTop = tape.scrollHeight;
}

async function loadHistory() {
  if (!state.runId) return;
  const data = await api(`/api/runs/${state.runId}/activity?limit=300`);
  const items = (data.items || []).slice().sort((a, b) => a.id - b.id);
  for (const a of items) appendActivity(a);
  const tape = $('tape');
  tape.scrollTop = tape.scrollHeight;
  updateTapeCount();
}

function connectStream() {
  if (!state.runId) return;
  if (state.es) state.es.close();
  const es = new EventSource(`/api/runs/${state.runId}/activity/stream`);
  state.es = es;

  es.addEventListener('activity', (ev) => {
    try {
      const a = JSON.parse(ev.data);
      state.connected = true;
      appendActivity(a);
      updateTapeCount();
    } catch (e) { console.warn('bad frame', e); }
  });
  es.onopen = () => { state.connected = true; updateTapeCount(); };
  es.onerror = () => { state.connected = false; updateTapeCount(); }; // 浏览器会自动重连
}

/* ── 详情抽屉 ───────────────────────────────────────── */

async function openDetail(a) {
  if (!a || !state.runId) return;
  $('drawer').hidden = false;
  $('drawer-title').textContent = `#${a.id} · ${a.kind}${a.tool ? ' · ' + a.tool : ''}`;
  $('drawer-body').textContent = a.summary || '';
  try {
    const d = await api(`/api/runs/${state.runId}/activity/${a.id}`);
    $('drawer-body').textContent = d.detail || d.summary || '(无正文)';
  } catch (e) {
    $('drawer-body').textContent = `${a.summary || ''}\n\n（详情读取失败：${e.message}）`;
  }
}

/* ── 交互 ───────────────────────────────────────────── */

function bindControls() {
  $('run-select').addEventListener('change', (e) => selectRun(Number(e.target.value)));

  document.querySelectorAll('.chip').forEach((btn) => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('.chip').forEach((b) => b.classList.remove('is-on'));
      btn.classList.add('is-on');
      state.filter = btn.dataset.filter;
      document.querySelectorAll('.tick').forEach((t) => {
        t.hidden = state.filter !== 'all' && t.dataset.cat !== state.filter;
      });
    });
  });

  $('btn-pause').addEventListener('click', () => control('pause'));
  $('btn-resume').addEventListener('click', () => control('resume'));
  $('btn-refresh').addEventListener('click', () => refreshAll());
  $('drawer-close').addEventListener('click', () => { $('drawer').hidden = true; });
  $('to-bottom').addEventListener('click', () => {
    state.stickBottom = true;
    const tape = $('tape');
    tape.scrollTop = tape.scrollHeight;
    $('to-bottom').hidden = true;
  });

  const tape = $('tape');
  tape.addEventListener('scroll', () => {
    const near = tape.scrollHeight - tape.scrollTop - tape.clientHeight < 80;
    state.stickBottom = near;
    $('to-bottom').hidden = near;
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') $('drawer').hidden = true;
  });
}

async function control(action) {
  if (!state.runId) return;
  try {
    await api(`/api/runs/${state.runId}/control`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action }),
    });
    toast(action === 'pause' ? '已暂停' : '已恢复');
    await refreshAll();
  } catch (e) { toast('操作失败：' + e.message); }
}

async function refreshAll() {
  await Promise.all([loadOverview(), loadSteps(), loadTokens()]);
  await loadRunsStatusOnly();
}

async function loadRunsStatusOnly() {
  try {
    const data = await api('/api/runs');
    state.runs = data.items || [];
    renderStatus();
  } catch {}
}

/* ── 启动 ───────────────────────────────────────────── */

(async function boot() {
  bindControls();
  try {
    await loadRuns();
  } catch (e) {
    toast('连接后端失败：' + e.message);
  }
  setInterval(() => { if (state.runId) { loadOverview(); loadTokens(); loadRunsStatusOnly(); } }, 5000);
  setInterval(() => { if (state.runId) loadSteps(); }, 8000);
})();
