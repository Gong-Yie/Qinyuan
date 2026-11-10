/* 图渲染（链路图 + 覆盖图）—— 不引第三方库，自己布局、自己画 SVG。
 *
 * 链路图布局（照 ARTEX 的做法，但代码自写）：
 *   1. DFS 三色标记找回边 → 断环得到 DAG；
 *   2. 拓扑序算最长路径 → 层号（layer[v] = max(layer[u]+1)）；
 *   3. 每层内按相邻层重心的平均值排序（4 轮来回 sweep，稳定排序）；
 *   4. 坐标：x = layer * 300，y 居中 + i * 104。
 *
 * 覆盖图：按父节点分组折叠（每 kind 默认 20 个，已测优先），再跑一个确定性的
 * 力导向（圆环初始位 + 固定迭代次数），所以每次刷新布局一致、不会"跳"。
 */

(function () {
  const KIND_COLOR = {
    goal: '#6366f1', plan: '#8b5cf6', step: '#a78bfa', intent: '#0ea5e9',
    fact: '#10b981', finding: '#ef4444', hint: '#f59e0b', digest: '#64748b',
    root_domain: '#6366f1', subdomain: '#0ea5e9', ip: '#f59e0b',
    service: '#10b981', endpoint: '#8b5cf6', app: '#ec4899',
  };
  const REL_COLOR = {
    spawns: '#6366f1', derived_from: '#8b5cf6', yields: '#10b981',
    proves: '#ef4444', covers: '#94a3b8', satisfies: '#f59e0b',
    realized_by: '#0ea5e9', depends_on: '#64748b',
  };
  const REL_LABEL = {
    spawns: '产出目标', derived_from: '派生', yields: '产出', proves: '证明',
    covers: '折叠', satisfies: '满足', realized_by: '派工', depends_on: '依赖',
  };
  const SVGNS = 'http://www.w3.org/2000/svg';
  const NODE_W = 240;
  const NODE_H = 84;

  function svgEl(tag, attrs) {
    const n = document.createElementNS(SVGNS, tag);
    for (const k in attrs || {}) n.setAttribute(k, attrs[k]);
    return n;
  }

  /* ── 视口：滚轮缩放 + 拖拽平移 ─────────────────────── */

  function attachViewport(svg) {
    const g = svg.querySelector('g.viewport');
    let scale = 1, tx = 0, ty = 0, drag = null;
    const apply = () => g.setAttribute('transform', `translate(${tx},${ty}) scale(${scale})`);

    svg.addEventListener('wheel', (e) => {
      e.preventDefault();
      const r = svg.getBoundingClientRect();
      const mx = e.clientX - r.left, my = e.clientY - r.top;
      const k = e.deltaY < 0 ? 1.12 : 1 / 1.12;
      const ns = Math.min(2.6, Math.max(0.2, scale * k));
      tx = mx - (mx - tx) * (ns / scale);
      ty = my - (my - ty) * (ns / scale);
      scale = ns;
      apply();
    }, { passive: false });

    svg.addEventListener('pointerdown', (e) => {
      if (e.target.closest('.gnode')) return;
      drag = { x: e.clientX - tx, y: e.clientY - ty };
      svg.setPointerCapture(e.pointerId);
      svg.classList.add('is-panning');
    });
    svg.addEventListener('pointermove', (e) => {
      if (!drag) return;
      tx = e.clientX - drag.x;
      ty = e.clientY - drag.y;
      apply();
    });
    const end = () => { drag = null; svg.classList.remove('is-panning'); };
    svg.addEventListener('pointerup', end);
    svg.addEventListener('pointercancel', end);

    return {
      reset() { scale = 1; tx = 0; ty = 0; apply(); },
      zoomBy(k) { scale = Math.min(2.6, Math.max(0.2, scale * k)); apply(); },
    };
  }

  function fitViewBox(svg, w, h, pad) {
    svg.setAttribute('viewBox', `${-pad} ${-pad} ${w + pad * 2} ${h + pad * 2}`);
  }

  /* ── 链路图布局 ─────────────────────────────────────── */

  function layoutExploration(nodes, edges) {
    const ids = new Set(nodes.map((n) => n.id));
    const es = edges.filter((e) => ids.has(e.src) && ids.has(e.dst) && e.src !== e.dst);
    const out = new Map(), inn = new Map();
    ids.forEach((id) => { out.set(id, []); inn.set(id, []); });
    for (const e of es) { out.get(e.src).push(e.dst); inn.get(e.dst).push(e.src); }

    // 1) DFS 三色断环：0 未访问 / 1 在栈 / 2 完成
    const color = new Map();
    ids.forEach((id) => color.set(id, 0));
    const backEdges = new Set();
    const stack = [];
    for (const start of ids) {
      if (color.get(start) !== 0) continue;
      stack.push({ id: start, i: 0 });
      color.set(start, 1);
      while (stack.length) {
        const top = stack[stack.length - 1];
        const outs = out.get(top.id) || [];
        if (top.i >= outs.length) { color.set(top.id, 2); stack.pop(); continue; }
        const next = outs[top.i++];
        const c = color.get(next);
        if (c === 1) backEdges.add(`${top.id}->${next}`);
        else if (c === 0) { color.set(next, 1); stack.push({ id: next, i: 0 }); }
      }
    }

    const dag = es.filter((e) => !backEdges.has(`${e.src}->${e.dst}`));
    const dagOut = new Map(), indeg = new Map();
    ids.forEach((id) => { dagOut.set(id, []); indeg.set(id, 0); });
    for (const e of dag) { dagOut.get(e.src).push(e.dst); indeg.set(e.dst, indeg.get(e.dst) + 1); }

    // 2) 拓扑序 + 最长路径分层
    const layer = new Map();
    ids.forEach((id) => layer.set(id, 0));
    const queue = [...ids].filter((id) => indeg.get(id) === 0);
    const order = [];
    while (queue.length) {
      const id = queue.shift();
      order.push(id);
      for (const d of dagOut.get(id)) {
        layer.set(d, Math.max(layer.get(d), layer.get(id) + 1));
        indeg.set(d, indeg.get(d) - 1);
        if (indeg.get(d) === 0) queue.push(d);
      }
    }
    // 有环残留（理论上不会）时按插入序补层
    for (const id of ids) if (!order.includes(id)) layer.set(id, 0);

    const maxLayer = Math.max(0, ...[...layer.values()]);
    const cols = [];
    for (let i = 0; i <= maxLayer; i++) cols.push([]);
    for (const id of ids) cols[layer.get(id)].push(id);

    // 3) 重心法排序：4 轮来回
    const posIndex = new Map();
    cols.forEach((col) => col.forEach((id, i) => posIndex.set(id, i)));
    const barycenter = (id, ref) => {
      const ns = (ref.get(id) || []).filter((x) => posIndex.has(x));
      if (!ns.length) return posIndex.get(id);
      return ns.reduce((a, x) => a + posIndex.get(x), 0) / ns.length;
    };
    for (let round = 0; round < 4; round++) {
      const forward = round % 2 === 0;
      const seq = forward ? cols : [...cols].reverse();
      for (const col of seq) {
        col.sort((a, b) => (barycenter(a, forward ? inn : out) - barycenter(b, forward ? inn : out)) || a - b);
        col.forEach((id, i) => posIndex.set(id, i));
      }
    }

    // 4) 坐标
    const gapX = 330, gapY = 122;
    const maxRows = Math.max(1, ...cols.map((c) => c.length));
    const cy = (maxRows * gapY) / 2;
    const pos = new Map();
    cols.forEach((col, ci) => {
      col.forEach((id, ri) => {
        pos.set(id, {
          x: ci * gapX,
          y: cy + (ri - (col.length - 1) / 2) * gapY,
          layer: ci,
        });
      });
    });
    return { pos, backEdges, cols, dagEdges: dag, edges: es };
  }

  /* ── digest 折叠（有 covers 边时）──────────────────── */

  function foldDigests(nodes, edges) {
    const byId = new Map(nodes.map((n) => [n.id, n]));
    const covers = edges.filter((e) => e.rel === 'covers' && byId.has(e.src) && byId.has(e.dst));
    if (!covers.length) return { nodes, edges, digests: [] };
    const hidden = new Set(covers.map((e) => e.dst));
    const digestOf = new Map();
    for (const e of covers) digestOf.set(e.dst, e.src);
    const visible = nodes.filter((n) => !hidden.has(n.id) || n.kind === 'digest');
    const remap = (id) => (hidden.has(id) ? digestOf.get(id) : id);
    const seen = new Set();
    const out = [];
    for (const e of edges) {
      if (e.rel === 'covers') continue;
      const s = remap(e.src), d = remap(e.dst);
      if (s === d) continue;
      const k = `${s}|${e.rel}|${d}`;
      if (seen.has(k)) continue;
      seen.add(k);
      out.push({ src: s, dst: d, rel: e.rel });
    }
    return { nodes: visible, edges: out, digests: covers };
  }

  /* ── 画链路图 ───────────────────────────────────────── */

  function renderExploration(host, data, onNode) {
    host.innerHTML = '';
    const folded = foldDigests(data.nodes || [], data.edges || []);
    if (!folded.nodes.length) {
      host.innerHTML = '<div class="empty"><strong>还没有图</strong>任务刚建，等 planner 产出目标与意图。</div>';
      return null;
    }
    const { pos, backEdges, edges, dagEdges } = layoutExploration(folded.nodes, folded.edges);
    const byId = new Map(folded.nodes.map((n) => [n.id, n]));

    let maxX = 0, maxY = 0;
    pos.forEach((p) => { maxX = Math.max(maxX, p.x); maxY = Math.max(maxY, p.y); });

    const wrap = document.createElement('div');
    wrap.className = 'graph-wrap';
    wrap.innerHTML = `
      <div class="graph-toolbar">
        <div class="legend">
          ${[['goal', '目标'], ['intent', '意图'], ['fact', '事实'], ['finding', '发现'], ['step', '步骤'], ['plan', '计划']]
            .map(([k, label]) => `<span><i style="background:${KIND_COLOR[k]}"></i>${label}</span>`).join('')}
        </div>
        <span class="spacer"></span>
        <span class="muted" id="graph-count"></span>
        <button class="btn btn-sm" data-zoom="in">＋</button>
        <button class="btn btn-sm" data-zoom="out">－</button>
        <button class="btn btn-sm" data-zoom="reset">适配</button>
      </div>
      <svg class="graph" role="img" aria-label="探索链路图"></svg>`;
    host.appendChild(wrap);

    const svg = wrap.querySelector('svg');
    const g = svgEl('g', { class: 'viewport' });
    const gEdges = svgEl('g', { class: 'gedges' });
    const gNodes = svgEl('g', { class: 'gnodes' });
    g.appendChild(gEdges);
    g.appendChild(gNodes);
    svg.appendChild(g);

    // 箭头
    const defs = svgEl('defs');
    defs.innerHTML = `<marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
      <path d="M 0 0 L 10 5 L 0 10 z" fill="context-stroke"></path></marker>`;
    svg.appendChild(defs);

    // 边
    for (const e of edges) {
      const a = pos.get(e.src), b = pos.get(e.dst);
      if (!a || !b) continue;
      const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2;
      const x2 = b.x, y2 = b.y + NODE_H / 2;
      const back = backEdges.has(`${e.src}->${e.dst}`);
      const dx = Math.max(50, Math.abs(x2 - x1) / 2);
      const d = `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
      const color = REL_COLOR[e.rel] || '#94a3b8';
      const path = svgEl('path', {
        d, fill: 'none', stroke: color, 'stroke-width': 1.3,
        'stroke-dasharray': back ? '4 4' : '',
        'marker-end': 'url(#arrow)',
        opacity: 0.75,
      });
      path.classList.add('gedge');
      path.appendChild(svgEl('title')).textContent = `${e.src} --${e.rel}--> ${e.dst}`;
      gEdges.appendChild(path);
      // 关系标签：放在两端中点上方，颜色跟关系色（跟随缩放）。
      const mx = (x1 + x2) / 2, my = (y1 + y2) / 2 - 6;
      const lbl = svgEl('text', {
        x: mx, y: my, 'text-anchor': 'middle', class: 'gedge-label',
        fill: color, 'font-size': 9.5, opacity: 0.9,
      });
      lbl.textContent = REL_LABEL[e.rel] || e.rel;
      gEdges.appendChild(lbl);
    }

    // 节点
    for (const [id, p] of pos) {
      const n = byId.get(id);
      if (!n) continue;
      const color = KIND_COLOR[n.kind] || '#94a3b8';
      const fo = svgEl('foreignObject', {
        x: p.x, y: p.y, width: NODE_W, height: NODE_H, class: 'gnode',
        'data-id': String(id),
      });
      const div = document.createElement('div');
      div.className = 'gnode-box';
      div.style.setProperty('--k', color);
      const running = n.state === 'running';
      if (running) div.classList.add('is-running');
      const summary = (n.summary || '').replace(/\s+/g, ' ').slice(0, 90) || '(无摘要)';
      div.innerHTML = `
        <div class="gnode-top"><span class="gnode-kind">${n.kind}</span>
          <span class="gnode-id">#${n.id}</span></div>
        <div class="gnode-summary">${escapeHtml(summary)}</div>
        <div class="gnode-state">${n.state || ''}</div>`;
      div.addEventListener('click', () => onNode && onNode(n));
      fo.appendChild(div);
      gNodes.appendChild(fo);
    }

    fitViewBox(svg, maxX + NODE_W, maxY + NODE_H, 60);
    const vp = attachViewport(svg);
    wrap.querySelector('[data-zoom="in"]').addEventListener('click', () => vp.zoomBy(1.25));
    wrap.querySelector('[data-zoom="out"]').addEventListener('click', () => vp.zoomBy(1 / 1.25));
    wrap.querySelector('[data-zoom="reset"]').addEventListener('click', () => vp.reset());
    wrap.querySelector('#graph-count').textContent =
      `${folded.nodes.length} 节点 / ${dagEdges.length} 边${folded.digests.length ? ' · 折叠 ' + folded.digests.length + ' 组' : ''}`;
    return wrap;
  }

  /* ── 覆盖图：折叠 + 确定性力导向 ───────────────────── */

  function foldChildren(nodes, edges, limit) {
    const children = new Map();
    for (const e of edges) {
      if (!children.has(e.dst)) children.set(e.dst, []);
      children.get(e.dst).push(e.src);
    }
    const byKey = new Map(nodes.map((n) => [n.key, n]));
    const visible = new Set(nodes.map((n) => n.key));
    const folds = [];
    const outEdges = [];
    const handled = new Set();
    for (const [parent, kids] of children) {
      const groups = new Map();
      for (const k of kids) {
        const n = byKey.get(k);
        if (!n) continue;
        if (!groups.has(n.kind)) groups.set(n.kind, []);
        groups.get(n.kind).push(k);
      }
      for (const [kind, list] of groups) {
        if (handled.has(parent + '|' + kind)) continue;
        handled.add(parent + '|' + kind);
        // 已测优先，其次按 key 稳定
        const sorted = [...list].sort((a, b) => {
          const ta = byKey.get(a).tested ? 0 : 1, tb = byKey.get(b).tested ? 0 : 1;
          return ta - tb || a.localeCompare(b);
        });
        if (sorted.length <= limit) continue;
        const keep = sorted.slice(0, limit);
        const rest = sorted.slice(limit);
        const key = `fold:${parent}:${kind}`;
        folds.push({
          key, parent, kind, hidden: rest,
          label: `还有 ${rest.length} 个 ${kind}`,
        });
        visible.add(key);
        outEdges.push({ src: key, dst: parent, fold: true });
        for (const k of rest) visible.delete(k);
      }
    }
    const keptNodes = nodes.filter((n) => visible.has(n.key));
    for (const f of folds) {
      keptNodes.push({
        key: f.key, kind: f.kind, label: f.label, tested: false, in_scope: true, fold: true,
      });
    }
    const keptEdges = edges.filter((e) => visible.has(e.src) && visible.has(e.dst));
    return { nodes: keptNodes, edges: keptEdges.concat(outEdges), folds };
  }

  function forceLayout(nodes, edges, width, height) {
    const n = nodes.length;
    const idx = new Map(nodes.map((nd, i) => [nd.key, i]));
    const R = Math.min(width, height) * 0.36;
    const xs = new Float64Array(n), ys = new Float64Array(n);
    for (let i = 0; i < n; i++) {
      const a = (i / n) * Math.PI * 2;
      xs[i] = width / 2 + Math.cos(a) * R;
      ys[i] = height / 2 + Math.sin(a) * R;
    }
    const links = edges.map((e) => [idx.get(e.src), idx.get(e.dst)]).filter(([a, b]) => a != null && b != null);
    const k = Math.sqrt((width * height) / Math.max(1, n));
    for (let iter = 0; iter < 260; iter++) {
      const t = 1 - iter / 260;
      const dx = new Float64Array(n), dy = new Float64Array(n);
      for (let i = 0; i < n; i++) {
        for (let j = i + 1; j < n; j++) {
          let vx = xs[i] - xs[j], vy = ys[i] - ys[j];
          let d2 = vx * vx + vy * vy;
          if (d2 < 1) { vx = 0.1 * (i - j); vy = 0.1 * (j - i); d2 = 1; }
          const f = (k * k) / d2;
          dx[i] += vx * f; dy[i] += vy * f;
          dx[j] -= vx * f; dy[j] -= vy * f;
        }
      }
      for (const [a, b] of links) {
        const vx = xs[a] - xs[b], vy = ys[a] - ys[b];
        const d = Math.max(1, Math.hypot(vx, vy));
        const f = (d * d) / k;
        dx[a] -= (vx / d) * f; dy[a] -= (vy / d) * f;
        dx[b] += (vx / d) * f; dy[b] += (vy / d) * f;
      }
      for (let i = 0; i < n; i++) {
        // 向心力，防止散开
        dx[i] += (width / 2 - xs[i]) * 0.02;
        dy[i] += (height / 2 - ys[i]) * 0.02;
        const d = Math.max(1, Math.hypot(dx[i], dy[i]));
        const step = Math.min(d, 24 * t);
        xs[i] += (dx[i] / d) * step;
        ys[i] += (dy[i] / d) * step;
        xs[i] = Math.max(40, Math.min(width - 40, xs[i]));
        ys[i] = Math.max(40, Math.min(height - 40, ys[i]));
      }
    }
    return new Map(nodes.map((nd, i) => [nd.key, { x: xs[i], y: ys[i] }]));
  }

  // radialLayout：覆盖图是"资产树"（根 → 服务 → 端点），同心圆分层比力导向好读得多。
  // 根在中心，depth=d 的节点排在第 d 圈上，按 key 稳定排序（刷新不跳）。
  function radialLayout(nodes, edges, width, height) {
    const children = new Map();
    const parentOf = new Map();
    for (const e of edges) {
      if (!children.has(e.dst)) children.set(e.dst, []);
      children.get(e.dst).push(e.src);
      parentOf.set(e.src, e.dst);
    }
    const roots = nodes.filter((n) => !parentOf.has(n.key)).map((n) => n.key);
    const depth = new Map();
    const queue = [];
    for (const r of roots) { depth.set(r, 0); queue.push(r); }
    while (queue.length) {
      const k = queue.shift();
      for (const c of children.get(k) || []) {
        if (depth.has(c)) continue;
        depth.set(c, depth.get(k) + 1);
        queue.push(c);
      }
    }
    for (const n of nodes) if (!depth.has(n.key)) depth.set(n.key, 1);

    const levels = new Map();
    for (const n of nodes) {
      const d = depth.get(n.key);
      if (!levels.has(d)) levels.set(d, []);
      levels.get(d).push(n.key);
    }
    const cx = width / 2, cy = height / 2;
    const pos = new Map();
    for (const [d, keys] of levels) {
      keys.sort();
      if (d === 0) {
        keys.forEach((k, i) => pos.set(k, { x: cx + (i - (keys.length - 1) / 2) * 26, y: cy }));
        continue;
      }
      const R = 140 + (d - 1) * 155;
      keys.forEach((k, i) => {
        const a = (i / keys.length) * Math.PI * 2 - Math.PI / 2;
        pos.set(k, { x: cx + Math.cos(a) * R, y: cy + Math.sin(a) * R });
      });
    }
    return pos;
  }

  // 端点标签只留 path（完整 URL 太长，一屏 26 个读不了）。
  function shortLabel(n) {
    const raw = n.label || '';
    if (n.kind === 'endpoint') {
      const m = raw.match(/^https?:\/\/[^/]+(\/.*)?$/);
      if (m) return m[1] || '/';
    }
    return raw;
  }

  function renderCoverage(host, data, onNode) {
    host.innerHTML = '';
    const folded = foldChildren(data.nodes || [], data.edges || [], 20);
    if (!folded.nodes.length) {
      host.innerHTML = '<div class="empty"><strong>范围内还没有实体</strong>先登记授权范围，worker 用 insert_assets 登记实体。</div>';
      return null;
    }
    const W = 1200, H = 640;
    const pos = radialLayout(folded.nodes, folded.edges, W, H);

    const wrap = document.createElement('div');
    wrap.className = 'graph-wrap';
    wrap.innerHTML = `
      <div class="graph-toolbar">
        <div class="legend">
          ${[['root_domain', '根域'], ['subdomain', '子域'], ['ip', 'IP'], ['service', '服务'], ['endpoint', '端点']]
            .map(([k, label]) => `<span><i style="background:${KIND_COLOR[k]}"></i>${label}</span>`).join('')}
          <span><i class="hollow"></i>范围内未测</span>
          <span class="muted">淡色 = 范围外（仅作上下文）</span>
        </div>
        <span class="spacer"></span>
        <span class="muted">${folded.nodes.length} 节点 · 折叠 ${folded.folds.length} 组</span>
        <button class="btn btn-sm" data-zoom="in">＋</button>
        <button class="btn btn-sm" data-zoom="out">－</button>
        <button class="btn btn-sm" data-zoom="reset">适配</button>
      </div>
      <svg class="graph" role="img" aria-label="资产覆盖图"></svg>`;
    host.appendChild(wrap);

    const svg = wrap.querySelector('svg');
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    const g = svgEl('g', { class: 'viewport' });
    const gEdges = svgEl('g', { class: 'gedges' });
    const gNodes = svgEl('g', { class: 'gnodes' });
    g.appendChild(gEdges); g.appendChild(gNodes);
    svg.appendChild(g);

    for (const e of folded.edges) {
      const a = pos.get(e.src), b = pos.get(e.dst);
      if (!a || !b) continue;
      const path = svgEl('path', {
        d: `M ${a.x} ${a.y} L ${b.x} ${b.y}`,
        stroke: '#c7c9d1', 'stroke-width': 1,
        'stroke-dasharray': e.fold ? '3 3' : '',
        fill: 'none', opacity: 0.8,
      });
      gEdges.appendChild(path);
    }

    for (const nd of folded.nodes) {
      const p = pos.get(nd.key);
      if (!p) continue;
      const color = KIND_COLOR[nd.kind] || '#94a3b8';
      const r = nd.fold ? 12 : nd.kind === 'root_domain' ? 18 : nd.kind === 'subdomain' || nd.kind === 'ip' ? 14 : 11;
      const node = svgEl('g', { class: 'cnode', transform: `translate(${p.x},${p.y})` });
      if (nd.in_scope === false) node.setAttribute('opacity', '0.45');
      const circle = svgEl('circle', {
        r,
        fill: nd.fold ? '#fff' : (nd.tested ? color : '#fff'),
        stroke: color,
        'stroke-width': nd.fold ? 1 : 1.6,
        'stroke-dasharray': nd.tested ? '' : '3 2',
      });
      node.appendChild(circle);
      // 标签只给"值得读"的节点：范围内 / 已测 / 根域·子域·IP·服务 / 折叠节点。
      // 其余（一堆同主机的端点）hover 看 title —— 全标标签会糊成一团。
      const showLabel = folded.nodes.length <= 30 || nd.in_scope || nd.tested || nd.fold;
      if (showLabel) {
        const label = svgEl('text', {
          x: 0, y: r + 13, 'text-anchor': 'middle', class: 'cnode-label',
        });
        label.textContent = shortLabel(nd).slice(0, 18);
        node.appendChild(label);
      }
      node.appendChild(svgEl('title')).textContent =
        (nd.label || nd.key) + (nd.in_scope === false ? '（范围外）' : '');
      node.addEventListener('click', () => onNode && onNode(nd));
      gNodes.appendChild(node);
    }

    const vp = attachViewport(svg);
    wrap.querySelector('[data-zoom="in"]').addEventListener('click', () => vp.zoomBy(1.25));
    wrap.querySelector('[data-zoom="out"]').addEventListener('click', () => vp.zoomBy(1 / 1.25));
    wrap.querySelector('[data-zoom="reset"]').addEventListener('click', () => vp.reset());
    return wrap;
  }

  function escapeHtml(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  window.QY_GRAPH = {
    renderExploration,
    renderCoverage,
    foldDigests,
    foldChildren,
    layoutExploration,
    REL_LABEL,
  };
})();

