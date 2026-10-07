// Mirante — painel em tempo real dos agentes.
// Sem dependências: SVG + EventSource. Estado vem de /api/stream (snapshot +
// updates); detalhe completo de um run vem de /api/runs/{id}.
(() => {
  'use strict';

  const params = new URLSearchParams(location.search);
  const WINDOW_MIN = +(params.get('janela') || 15);
  const WIN = WINDOW_MIN * 60e3;
  const MAX_RUNS = 600;
  const FEED_MAX = 40;
  const SEG_MS = 520; // duração de cada trecho da animação do pacote
  const NS = 'http://www.w3.org/2000/svg';

  const $ = (id) => document.getElementById(id);
  const svg = $('map');
  const L = {};
  for (const k of ['edges', 'packets', 'nodes', 'overlay']) {
    L[k] = document.createElementNS(NS, 'g');
    svg.appendChild(L[k]);
  }

  const state = {
    runs: new Map(),
    seq: 0,
    topo: new Map(), // "agent|server|tool" -> declared
    agentOrder: [],
    seenFlags: new Set(),
  };
  const nodes = new Map(); // id -> node
  const edges = new Map(); // "a>b" -> {path, last}
  const inflight = new Map(); // span_id -> {nodeId, start, out: Promise}
  let topoSig = '';

  // ---------- util ----------
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
  const nf1 = new Intl.NumberFormat('pt-BR', { maximumFractionDigits: 1, minimumFractionDigits: 1 });
  function fmtMs(ms) {
    if (ms == null || isNaN(ms)) return '—';
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return nf1.format(ms / 1000) + ' s';
    return Math.floor(ms / 60000) + 'min ' + Math.round((ms % 60000) / 1000) + 's';
  }
  const pct = (n, d) => (d ? Math.round((100 * n) / d) + '%' : '—');
  function quant(arr, q) {
    if (!arr.length) return null;
    const s = [...arr].sort((a, b) => a - b);
    return s[Math.min(s.length - 1, Math.floor(q * s.length))];
  }
  const t = (iso) => new Date(iso).getTime();
  const hhmmss = (ms) => new Date(ms).toLocaleTimeString('pt-BR');
  function svgEl(tag, attrs, parent) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  const srv = (s) => s || 'local';
  const nid = { a: (a) => 'a:' + a, s: (s) => 's:' + srv(s), t: (s, tool) => 't:' + srv(s) + '/' + tool };

  function agentColor(agent) {
    let i = state.agentOrder.indexOf(agent);
    if (i < 0) { state.agentOrder.push(agent); i = state.agentOrder.length - 1; }
    return `var(--agent-${(i % 8) + 1})`;
  }

  function worst(run) {
    let w = '';
    const chk = (fs) => (fs || []).forEach((f) => {
      if (f.level === 'hallucination') w = 'hallucination';
      else if (f.level === 'uncertain' && !w) w = 'uncertain';
    });
    chk(run.flags);
    (run.steps || []).forEach((s) => chk(s.flags));
    return w;
  }
  function statusIcon(status, w) {
    if (w === 'hallucination') return '<span class="st hallucination" title="alucinação">!</span>';
    if (w === 'uncertain') return '<span class="st uncertain" title="incerteza">?</span>';
    if (status === 'running') return '<span class="st running" title="em andamento"></span>';
    if (status === 'error') return '<span class="st error" title="erro">×</span>';
    return '<span class="st ok" title="ok"></span>';
  }

  // ---------- topologia & layout ----------
  function addTopo(agent, server, tool, declared) {
    const k = agent + '|' + srv(server) + '|' + tool;
    const prev = state.topo.get(k);
    if (prev === undefined || (declared && !prev)) {
      state.topo.set(k, !!declared || !!prev);
      return true;
    }
    return false;
  }

  function graph() {
    const agents = new Set(), servers = new Map(), aEdges = new Set();
    for (const [k, declared] of state.topo) {
      const [a, s, tool] = k.split('|');
      agents.add(a);
      if (!servers.has(s)) servers.set(s, new Map());
      const tools = servers.get(s);
      tools.set(tool, (tools.get(tool) || false) || declared);
      aEdges.add(a + '|' + s);
    }
    for (const r of state.runs.values()) agents.add(r.agent);
    return { agents: [...agents].filter(Boolean), servers, aEdges };
  }

  function layout(animate = true) {
    const W = svg.clientWidth || 1200, H = svg.clientHeight || 700;
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    const { agents, servers, aEdges } = graph();
    let cx = W / 2, cy = H / 2 - 14;
    let rx = Math.max(160, W / 2 - 150), ry = Math.max(120, H / 2 - 70);

    const sNames = [...servers.keys()].sort();
    const total = sNames.reduce((n, s) => n + Math.max(1.5, servers.get(s).size), 0) || 1;
    // poucas tools: leque centrado embaixo; muitas: círculo inteiro
    const span = Math.min(2 * Math.PI, Math.max(Math.PI * 0.75, 0.55 * total));
    if (span < 2 * Math.PI - 1e-6) { cy = Math.max(90, H * 0.22); ry = Math.max(120, H - cy - 90); }
    const target = new Map();
    const sAngle = new Map();
    let a0 = span >= 2 * Math.PI - 1e-6 ? -Math.PI / 2 : Math.PI / 2 - span / 2;
    for (const s of sNames) {
      const tools = [...servers.get(s).keys()].sort();
      const sector = (span * Math.max(1.5, tools.length)) / total;
      const mid = a0 + sector / 2;
      sAngle.set(s, mid);
      target.set(nid.s(s), { x: cx + rx * 0.6 * Math.cos(mid), y: cy + ry * 0.6 * Math.sin(mid), kind: 'server', label: s, server: s, angle: mid });
      tools.forEach((tool, i) => {
        const ang = a0 + (sector * (i + 0.5)) / tools.length;
        target.set(nid.t(s, tool), {
          x: cx + rx * Math.cos(ang), y: cy + ry * Math.sin(ang), kind: 'tool', label: tool, server: s, tool,
          ghost: !servers.get(s).get(tool), angle: ang,
        });
      });
      a0 += sector;
    }

    // agentes: ordenados pela direção média dos servers que usam, num anel interno
    const mean = (a) => {
      let x = 0, y = 0;
      for (const s of sNames) if (aEdges.has(a + '|' + s)) { x += Math.cos(sAngle.get(s)); y += Math.sin(sAngle.get(s)); }
      return Math.atan2(y, x);
    };
    const ag = agents.map((a) => ({ a, m: mean(a) })).sort((p, q) => p.m - q.m);
    const ra = ag.length > 1 ? Math.min(rx, ry) * 0.17 + ag.length * 4 : 0;
    ag.forEach(({ a }, i) => {
      const ang = (ag[0]?.m ?? 0) + (2 * Math.PI * i) / ag.length;
      target.set(nid.a(a), { x: cx + ra * 1.6 * Math.cos(ang), y: cy + ra * Math.sin(ang), kind: 'agent', label: a, agent: a });
    });

    // remove o que sumiu
    for (const [id, n] of nodes) if (!target.has(id)) { n.g.remove(); nodes.delete(id); }
    for (const [id, tg] of target) {
      let n = nodes.get(id);
      if (!n) {
        n = buildNode(id, tg);
        n.x = n.fx = tg.kind === 'agent' ? tg.x : cx;
        n.y = n.fy = tg.kind === 'agent' ? tg.y : cy;
        nodes.set(id, n);
      } else if (n.ghost !== !!tg.ghost) {
        n.g.classList.toggle('ghost', !!tg.ghost);
        n.ghost = !!tg.ghost;
      }
      Object.assign(n, { tx: tg.x, ty: tg.y, angle: tg.angle, fx: n.x, fy: n.y });
      placeLabel(n);
    }
    for (const n of nodes.values()) if (n.kind === 'agent') L.nodes.appendChild(n.g); // agentes por cima

    // arestas
    const want = new Set();
    for (const k of aEdges) { const [a, s] = k.split('|'); want.add(nid.a(a) + '>' + nid.s(s)); }
    for (const s of sNames) for (const tool of servers.get(s).keys()) want.add(nid.s(s) + '>' + nid.t(s, tool));
    for (const [k, e] of edges) if (!want.has(k)) { e.path.remove(); edges.delete(k); }
    for (const k of want) {
      if (!edges.has(k)) {
        const [, to] = k.split('>');
        const path = svgEl('path', { class: 'edge' + (nodes.get(to)?.ghost ? ' ghost' : '') }, L.edges);
        edges.set(k, { path, last: 0 });
      } else {
        const [, to] = k.split('>');
        edges.get(k).path.classList.toggle('ghost', !!nodes.get(to)?.ghost);
      }
    }
    tween(animate ? 650 : 0);
  }

  let tweenRaf = 0;
  function tween(ms) {
    cancelAnimationFrame(tweenRaf);
    const t0 = performance.now();
    const step = (now) => {
      const p = ms ? Math.max(0, Math.min(1, (now - t0) / ms)) : 1;
      const e = 1 - Math.pow(1 - p, 3);
      for (const n of nodes.values()) {
        n.x = n.fx + (n.tx - n.fx) * e;
        n.y = n.fy + (n.ty - n.fy) * e;
        n.g.setAttribute('transform', `translate(${n.x.toFixed(1)},${n.y.toFixed(1)})`);
      }
      drawEdges();
      if (p < 1) tweenRaf = requestAnimationFrame(step);
    };
    tweenRaf = requestAnimationFrame(step);
  }

  function drawEdges() {
    for (const [k, e] of edges) {
      const [from, to] = k.split('>');
      const a = nodes.get(from), b = nodes.get(to);
      if (!a || !b) continue;
      const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2, dx = b.x - a.x, dy = b.y - a.y;
      const c = 0.12;
      e.path.setAttribute('d', `M${a.x},${a.y} Q${mx - dy * c},${my + dx * c} ${b.x},${b.y}`);
    }
  }

  function buildNode(id, tg) {
    const g = svgEl('g', { class: `node ${tg.kind}${tg.ghost ? ' ghost' : ''}`, tabindex: 0, role: 'button' }, L.nodes);
    const n = { id, g, kind: tg.kind, label: tg.label, agent: tg.agent, server: tg.server, tool: tg.tool, ghost: !!tg.ghost };
    if (tg.kind === 'tool') {
      n.r = 10;
      svgEl('circle', { class: 'hit', r: 26 }, g);
      n.spin = svgEl('circle', { class: 'spin', r: 16, stroke: 'var(--warn)' }, g);
      n.body = svgEl('circle', { class: 'body', r: n.r }, g);
      n.flash = svgEl('circle', { class: 'flash', r: n.r + 2 }, g);
    } else {
      const w = Math.max(70, tg.label.length * (tg.kind === 'agent' ? 10.5 : 8.2) + 30);
      const h = tg.kind === 'agent' ? 46 : 32;
      n.w = w; n.h = h; n.r = h / 2;
      svgEl('rect', { class: 'hit', x: -w / 2 - 8, y: -h / 2 - 8, width: w + 16, height: h + 16 }, g);
      n.spin = svgEl('rect', { class: 'spin', x: -w / 2 - 6, y: -h / 2 - 6, width: w + 12, height: h + 12, rx: h / 2 + 6, stroke: tg.kind === 'agent' ? agentColor(tg.agent) : 'var(--server)' }, g);
      n.body = svgEl('rect', { class: 'body', x: -w / 2, y: -h / 2, width: w, height: h, rx: h / 2 }, g);
      n.flash = svgEl('rect', { class: 'flash', x: -w / 2, y: -h / 2, width: w, height: h, rx: h / 2 }, g);
      if (tg.kind === 'agent') n.body.style.stroke = agentColor(tg.agent);
    }
    n.lbl = svgEl('text', { class: 'lbl' }, g);
    n.lbl.textContent = tg.label + (tg.ghost ? ' (inexistente)' : '');
    n.sub = svgEl('text', { class: 'sub' }, g);
    n.timer = svgEl('text', { class: 'timer', 'text-anchor': 'middle', y: -(n.r + 10) }, g);
    n.badge = svgEl('g', { class: 'badge', style: 'display:none' }, g);
    svgEl('circle', { r: 9 }, n.badge);
    n.badgeTxt = svgEl('text', {}, n.badge);
    const bx = tg.kind === 'tool' ? n.r * 0.85 : n.w / 2 - 4, by = tg.kind === 'tool' ? -n.r * 0.85 : -n.h / 2 + 2;
    n.badge.setAttribute('transform', `translate(${bx},${by})`);
    g.addEventListener('click', () => openNode(id));
    g.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') openNode(id); });
    return n;
  }

  function placeLabel(n) {
    if (n.kind === 'tool') {
      const c = Math.cos(n.angle), s = Math.sin(n.angle);
      let anchor = 'middle', x = 0, y = 0;
      if (c > 0.35) { anchor = 'start'; x = 18; y = 4; }
      else if (c < -0.35) { anchor = 'end'; x = -18; y = 4; }
      else { y = s > 0 ? 30 : -34; }
      for (const [el, dy] of [[n.lbl, 0], [n.sub, 16]]) {
        el.setAttribute('text-anchor', anchor);
        el.setAttribute('x', x);
        el.setAttribute('y', y + dy);
      }
      if (Math.abs(c) <= 0.35 && s < 0) { n.lbl.setAttribute('y', y - 14); n.sub.setAttribute('y', y + 2); }
    } else {
      n.lbl.setAttribute('text-anchor', 'middle');
      n.lbl.setAttribute('dominant-baseline', 'central');
      n.sub.setAttribute('text-anchor', 'middle');
      n.sub.setAttribute('y', n.h / 2 + 17);
    }
  }

  // ---------- animação ----------
  function edgePath(from, to) {
    const e = edges.get(from + '>' + to);
    if (e) { e.last = Date.now(); return { path: e.path, rev: false }; }
    const r = edges.get(to + '>' + from);
    if (r) { r.last = Date.now(); return { path: r.path, rev: true }; }
    return null;
  }

  function travel(segs, color, big) {
    segs = segs.filter(Boolean);
    if (!segs.length || document.hidden) return Promise.resolve();
    return new Promise((resolve) => {
      const dot = svgEl('circle', { r: big ? 7 : 5.5, class: 'packet', fill: color, style: `color:${color}` }, L.packets);
      const t0 = performance.now();
      const total = SEG_MS * segs.length;
      // rAF congela com a aba/painel oculto: garante que a promessa resolve mesmo assim
      const guard = setTimeout(() => { dot.remove(); resolve(); }, total + 400);
      const frame = (now) => {
        // timestamp do rAF pode ser anterior ao t0 (início do frame) → clamp em 0
        const p = Math.max(0, Math.min(1, (now - t0) / total));
        const idx = Math.min(segs.length - 1, Math.floor(p * segs.length));
        let lp = p * segs.length - idx;
        lp = lp < 0.5 ? 2 * lp * lp : 1 - Math.pow(-2 * lp + 2, 2) / 2;
        const { path, rev } = segs[idx];
        const len = path.getTotalLength();
        const pt = path.getPointAtLength(len * (rev ? 1 - lp : lp));
        dot.setAttribute('cx', pt.x);
        dot.setAttribute('cy', pt.y);
        if (p < 1) requestAnimationFrame(frame);
        else { clearTimeout(guard); dot.remove(); resolve(); }
      };
      requestAnimationFrame(frame);
    });
  }

  function flash(nodeId, color) {
    const n = nodes.get(nodeId);
    if (!n) return;
    n.flash.style.stroke = color;
    n.flash.classList.remove('go');
    void n.flash.getBBox();
    n.flash.classList.add('go');
  }

  const bubbles = new Map();
  function bubble(agent, text) {
    const n = nodes.get(nid.a(agent));
    if (!n) return;
    let b = bubbles.get(agent);
    if (!b) {
      b = { g: svgEl('g', { class: 'bubble' }, L.overlay) };
      b.rect = svgEl('rect', { rx: 8, height: 26 }, b.g);
      b.text = svgEl('text', { y: 17, x: 10 }, b.g);
      bubbles.set(agent, b);
    }
    b.text.textContent = text;
    const w = b.text.getComputedTextLength() + 20;
    b.rect.setAttribute('width', w);
    b.g.setAttribute('transform', `translate(${n.x - w / 2},${n.y - n.h / 2 - 40})`);
    b.g.style.opacity = 1;
    clearTimeout(b.to);
    b.to = setTimeout(() => (b.g.style.opacity = 0), 3200);
  }

  // ---------- ingestão de updates ----------
  function onSnapshot(data) {
    state.runs.clear();
    state.topo.clear();
    state.seq = data.seq || 0;
    for (const e of data.topology || []) addTopo(e.agent, e.server, e.tool, e.declared);
    for (const r of (data.runs || []).reverse()) { agentColor(r.agent); state.runs.set(r.id, r); }
    for (const r of state.runs.values()) for (const k of flagKeys(r)) state.seenFlags.add(k.key);
    inflight.clear();
    layout(false);
    renderFeed();
    tick(true);
  }

  function flagKeys(run) {
    const out = [];
    (run.flags || []).forEach((f) => out.push({ key: `${run.id}||${f.code}|${f.reason}`, f, run, step: null }));
    (run.steps || []).forEach((s) => (s.flags || []).forEach((f) => out.push({ key: `${run.id}|${s.span_id}|${f.code}|${f.reason}`, f, run, step: s })));
    return out;
  }

  function onUpdate(u) {
    if (u.seq <= state.seq) return;
    state.seq = u.seq;
    const e = u.event, run = u.run;
    if (e.type === 'tools') { // agente conectou num MCP server: só topologia
      agentColor(e.agent);
      let changed = false;
      for (const tl of e.tools || []) changed = addTopo(e.agent, tl.server, tl.name, true) || changed;
      if (changed || !nodes.has(nid.a(e.agent))) { layout(true); flash(nid.a(e.agent), agentColor(e.agent)); }
      return;
    }
    agentColor(run.agent);
    state.runs.set(run.id, run);
    if (state.runs.size > MAX_RUNS) state.runs.delete(state.runs.keys().next().value);

    let topoChanged = false;
    if (e.type === 'run_start') for (const tl of e.tools || []) topoChanged = addTopo(run.agent, tl.server, tl.name, true) || topoChanged;
    if (e.type === 'tool_call' || e.type === 'tool_result') {
      const k = run.agent + '|' + srv(e.server) + '|' + e.tool;
      if (!state.topo.has(k)) topoChanged = addTopo(run.agent, e.server, e.tool, false) || topoChanged;
    }
    if (!nodes.has(nid.a(run.agent))) topoChanged = true;
    const sig = [...state.topo.keys()].sort().join(',') + '#' + [...new Set([...state.runs.values()].map((r) => r.agent))].sort().join(',');
    if (topoChanged || sig !== topoSig) { topoSig = sig; layout(true); }

    const aId = nid.a(run.agent);
    const color = agentColor(run.agent);
    switch (e.type) {
      case 'run_start':
        flash(aId, color);
        bubble(run.agent, '◂ ' + (e.input || '').slice(0, 48));
        break;
      case 'decision': {
        const conf = e.confidence != null ? ` (${Math.round(e.confidence * 100)}%)` : '';
        bubble(run.agent, (e.chosen && e.chosen.length ? '→ ' + e.chosen.join(', ') : '✓ respondendo') + conf);
        break;
      }
      case 'tool_call': {
        const sId = nid.s(e.server), tId = nid.t(e.server, e.tool);
        const step = run.steps.find((s) => s.span_id === e.span_id);
        const bad = (step?.flags || []).some((f) => f.level === 'hallucination');
        const out = travel([edgePath(aId, sId), edgePath(sId, tId)], bad ? 'var(--crit)' : color, bad)
          .then(() => { const n = nodes.get(tId); if (n && inflight.has(e.span_id)) n.g.classList.add('busy'); });
        inflight.set(e.span_id, { nodeId: tId, start: Date.now(), out });
        break;
      }
      case 'tool_result': {
        const sId = nid.s(e.server), tId = nid.t(e.server, e.tool);
        const f = inflight.get(e.span_id);
        const ok = !e.error;
        // o timer para já (dado real); só a animação de volta espera a de ida
        inflight.delete(e.span_id);
        (f ? f.out : Promise.resolve()).then(() => new Promise((r) => setTimeout(r, 120))).then(() => {
          const n = nodes.get(tId);
          if (n && ![...inflight.values()].some((x) => x.nodeId === tId)) { n.g.classList.remove('busy'); n.timer.textContent = ''; }
          flash(tId, ok ? 'var(--good)' : 'var(--serious)');
          return travel([edgePath(tId, sId), edgePath(sId, aId)], ok ? 'var(--good)' : 'var(--serious)');
        });
        break;
      }
      case 'run_end':
        flash(aId, run.status === 'error' ? 'var(--serious)' : 'var(--good)');
        break;
    }

    // flags novas → ripple vermelho/amarelo + ticker
    for (const fk of flagKeys(run)) {
      if (state.seenFlags.has(fk.key)) continue;
      state.seenFlags.add(fk.key);
      const c = fk.f.level === 'hallucination' ? 'var(--crit)' : 'var(--warn)';
      const target = fk.step && fk.step.kind === 'tool' ? nid.t(fk.step.server, fk.step.tool) : aId;
      setTimeout(() => flash(target, c), fk.step?.kind === 'tool' ? SEG_MS * 2 : 0);
    }

    upsertCard(run);
    if (drawer.run === run.id) refreshRunSoon();
    scheduleTick();
  }

  // ---------- métricas, badges, KPIs ----------
  let tickPending = false;
  function scheduleTick() {
    if (tickPending) return;
    tickPending = true;
    setTimeout(() => { tickPending = false; tick(); }, 400);
  }

  function windowRuns() {
    const now = Date.now();
    return [...state.runs.values()].filter((r) => now - t(r.start) <= WIN);
  }

  function collect() {
    const runs = windowRuns();
    const per = new Map();
    const get = (id) => { if (!per.has(id)) per.set(id, { calls: [], runs: [], hal: 0, unc: 0, lastFlag: 0 }); return per.get(id); };
    const bump = (p, fs, when) => (fs || []).forEach((f) => {
      if (f.level === 'hallucination') p.hal++; else p.unc++;
      p.lastFlag = Math.max(p.lastFlag, when);
    });
    for (const r of runs) {
      const a = get(nid.a(r.agent));
      a.runs.push(r);
      // no agente o badge conta REQUISIÇÕES com sinal (não cada flag), igual aos KPIs
      const w = worst(r);
      if (w) bump(a, [{ level: w }], t(r.start) + (r.duration_ms || 0));
      for (const s of r.steps || []) {
        if (s.kind === 'decision') continue;
        const tp = get(nid.t(s.server, s.tool)), sp = get(nid.s(s.server));
        tp.calls.push({ run: r, step: s }); sp.calls.push({ run: r, step: s });
        bump(tp, s.flags, t(s.start)); bump(sp, s.flags, t(s.start));
      }
    }
    return { runs, per };
  }

  function tick() {
    const { runs, per } = collect();
    const now = Date.now();
    for (const n of nodes.values()) {
      const p = per.get(n.id) || { calls: [], runs: [], hal: 0, unc: 0, lastFlag: 0 };
      if (n.kind === 'agent') {
        const d = p.runs.filter((r) => r.status !== 'running').map((r) => r.duration_ms);
        const running = p.runs.filter((r) => r.status === 'running').length;
        n.sub.textContent = `${p.runs.length} req · p95 ${fmtMs(quant(d, 0.95))}` + (running ? ` · ${running} ativas` : '');
        n.g.classList.toggle('busy', running > 0);
      } else {
        const d = p.calls.filter((c) => c.step.status !== 'running').map((c) => c.step.duration_ms);
        const errs = p.calls.filter((c) => c.step.status === 'error').length;
        n.sub.textContent = p.calls.length
          ? `${p.calls.length}× · p95 ${fmtMs(quant(d, 0.95))}` + (errs ? ` · ${errs} erro${errs > 1 ? 's' : ''}` : '')
          : n.kind === 'tool' ? 'sem chamadas' : '';
      }
      const lvl = p.hal ? 'hal' : p.unc ? 'unc' : '';
      n.badge.style.display = lvl ? '' : 'none';
      n.badge.setAttribute('class', `badge ${lvl}${now - p.lastFlag < 60e3 ? ' recent' : ''}`);
      n.badgeTxt.textContent = lvl === 'hal' ? `${p.hal}` : `${p.unc}`;
      n.g.setAttribute('aria-label', `${n.label}: ${n.sub.textContent}${p.hal ? `, ${p.hal} alucinações` : ''}${p.unc ? `, ${p.unc} incertezas` : ''}`);
    }
    for (const e of edges.values()) e.path.classList.toggle('hot', now - e.last < 8000);

    // KPIs
    const done = runs.filter((r) => r.status !== 'running');
    const dur = done.map((r) => r.duration_ms);
    const steps = runs.flatMap((r) => (r.steps || []).filter((s) => s.kind === 'tool'));
    const terr = steps.filter((s) => s.status === 'error').length;
    const hal = runs.filter((r) => worst(r) === 'hallucination').length;
    const unc = runs.filter((r) => worst(r) === 'uncertain').length;
    const lastMin = runs.filter((r) => now - t(r.start) < 60e3).length;
    $('kpis').innerHTML = [
      kpi('Requisições', runs.length, `${lastMin}/min agora`),
      kpi('Em andamento', runs.length - done.length, `${inflight.size} tools em voo`),
      kpi('Latência p50 / p95', `${fmtMs(quant(dur, 0.5))}`, `p95 ${fmtMs(quant(dur, 0.95))}`),
      kpi('Erro de tool', pct(terr, steps.length), `${terr} de ${steps.length} chamadas`),
      kpi('Incerteza', `<span class="st uncertain ic">?</span>${unc}`, pct(unc, runs.length) + ' das requisições', 'unc' + (unc ? ' has' : '')),
      kpi('Alucinação', `<span class="st hallucination ic">!</span>${hal}`, pct(hal, runs.length) + ' das requisições', 'hal' + (hal ? ' has' : '')),
    ].join('');
    $('window-lbl').textContent = `janela: últimos ${WINDOW_MIN} min`;

    renderTicker(runs);
  }
  const kpi = (k, v, s, cls = '') => `<div class="kpi ${cls}"><div class="k">${k}</div><div class="v">${v}</div><div class="s">${s}</div></div>`;

  function renderTicker(runs) {
    const items = runs.flatMap(flagKeys)
      .map((x) => ({ ...x, when: x.step ? t(x.step.start) : t(x.run.start) + (x.run.duration_ms || 0) }))
      .sort((a, b) => b.when - a.when)
      .slice(0, 10);
    const el = $('ticker');
    const sig = items.map((x) => x.key).join(',');
    if (sig === renderTicker.sig) return; // re-render reinicia a animação de entrada
    renderTicker.sig = sig;
    if (!items.length) { el.innerHTML = '<span class="muted">nenhum alerta na janela</span>'; return; }
    el.innerHTML = items.map((x) => `<span class="tk" data-run="${esc(x.run.id)}" data-span="${esc(x.step?.span_id || '')}">
      <span class="st ${x.f.level}">${x.f.level === 'hallucination' ? '!' : '?'}</span>
      <b>${esc(x.run.agent)}</b>${x.step?.tool ? ` · <code>${esc(x.step.tool)}</code>` : ''}
      <span class="why">${esc(x.f.code)}: ${esc(x.f.reason.slice(0, 90))}</span>
      <span class="muted">${hhmmss(x.when)}</span></span>`).join('');
  }
  $('ticker').addEventListener('click', (ev) => {
    const tk = ev.target.closest('.tk');
    if (tk) openRun(tk.dataset.run, tk.dataset.span);
  });

  // timers ao vivo (tools em voo + runs em andamento)
  setInterval(() => {
    const now = Date.now();
    const per = new Map();
    for (const f of inflight.values()) per.set(f.nodeId, Math.max(per.get(f.nodeId) || 0, now - f.start));
    for (const n of nodes.values()) if (n.kind === 'tool') n.timer.textContent = per.has(n.id) ? fmtMs(per.get(n.id)) : '';
    document.querySelectorAll('[data-live-start]').forEach((el) => (el.textContent = fmtMs(now - +el.dataset.liveStart)));
  }, 250);
  setInterval(tick, 5000);

  // ---------- feed ----------
  function cardHTML(r) {
    const w = worst(r);
    const chips = (r.steps || []).filter((s) => s.kind === 'tool').map((s) => {
      const f = (s.flags || []).some((x) => x.level === 'hallucination') ? 'hallucination' : (s.flags || []).length ? 'uncertain' : '';
      return `<span class="chip ${s.status}">${f ? `<span class="st ${f}">${f === 'hallucination' ? '!' : '?'}</span>` : ''}${esc(s.tool)}</span>`;
    }).join('');
    const dur = r.status === 'running'
      ? `<span class="dur" data-live-start="${t(r.start)}">${fmtMs(Date.now() - t(r.start))}</span>`
      : `<span class="dur">${fmtMs(r.duration_ms)}</span>`;
    return `<div class="card-top">${statusIcon(r.status, w)}<span class="ag">${esc(r.agent)}</span>${r.user ? `<span>· ${esc(r.user)}</span>` : ''}${dur}</div>
      <p class="q">${esc(r.input || '(sem entrada)')}</p><div class="chips">${chips}</div>`;
  }
  function upsertCard(r) {
    const feed = $('feed');
    let li = feed.querySelector(`[data-id="${CSS.escape(r.id)}"]`);
    if (!li) {
      li = document.createElement('li');
      li.className = 'card';
      li.dataset.id = r.id;
      li.addEventListener('click', () => openRun(r.id));
      feed.prepend(li);
      while (feed.children.length > FEED_MAX) feed.lastElementChild.remove();
    }
    li.style.setProperty('--c', agentColor(r.agent));
    li.classList.toggle('hal', worst(r) === 'hallucination');
    li.innerHTML = cardHTML(r);
  }
  function renderFeed() {
    $('feed').innerHTML = '';
    [...state.runs.values()].sort((a, b) => t(a.start) - t(b.start)).slice(-FEED_MAX).forEach(upsertCard);
  }

  // ---------- drawer ----------
  const drawer = { el: $('drawer'), run: null, node: null };
  function openDrawer(title, body) {
    $('drawer-title').innerHTML = title;
    $('drawer-body').innerHTML = body;
    drawer.el.classList.add('open');
    drawer.el.setAttribute('aria-hidden', 'false');
  }
  function closeDrawer() {
    drawer.el.classList.remove('open');
    drawer.el.setAttribute('aria-hidden', 'true');
    drawer.run = drawer.node = null;
    nodes.forEach((n) => n.g.classList.remove('sel'));
  }
  $('drawer-close').addEventListener('click', closeDrawer);

  function openNode(id) {
    const n = nodes.get(id);
    if (!n) return;
    drawer.run = null;
    drawer.node = id;
    nodes.forEach((x) => x.g.classList.toggle('sel', x.id === id));
    const { per } = collect();
    const p = per.get(id) || { calls: [], runs: [], hal: 0, unc: 0 };
    const kindLbl = { agent: 'Agente', server: 'MCP server', tool: 'Tool' }[n.kind];

    if (n.kind === 'agent') {
      const runs = [...p.runs].sort((a, b) => t(b.start) - t(a.start));
      const d = runs.filter((r) => r.status !== 'running').map((r) => r.duration_ms);
      const hal = runs.filter((r) => worst(r) === 'hallucination').length, unc = runs.filter((r) => worst(r) === 'uncertain').length;
      const toolsUsed = runs.reduce((m, r) => m + (r.steps || []).filter((s) => s.kind === 'tool').length, 0);
      openDrawer(`<h3>${esc(n.label)}</h3><div class="meta">${kindLbl} · últimos ${WINDOW_MIN} min</div>`,
        `<div class="stats">${[
          stat('Requisições', runs.length), stat('p50', fmtMs(quant(d, 0.5))), stat('p95', fmtMs(quant(d, 0.95))),
          stat('Tools / req', runs.length ? nf1.format(toolsUsed / runs.length) : '—'),
          stat('Incerteza', pct(unc, runs.length)), stat('Alucinação', pct(hal, runs.length)),
        ].join('')}</div>
        <h4>Duração das últimas requisições</h4>${spark(runs.slice(0, 50).reverse().map((r) => ({ v: r.duration_ms, cls: worst(r) === 'hallucination' ? 'flag' : r.status === 'error' ? 'error' : '' })))}
        <h4>Requisições</h4>
        <table class="calls"><thead><tr><th></th><th>hora</th><th>entrada</th><th>tools</th><th class="num">duração</th></tr></thead><tbody>
        ${runs.slice(0, 60).map((r) => `<tr data-run="${esc(r.id)}"><td>${statusIcon(r.status, worst(r))}</td><td class="num">${hhmmss(t(r.start))}</td>
          <td>${esc((r.input || '').slice(0, 90))}</td><td>${(r.steps || []).filter((s) => s.kind === 'tool').map((s) => `<code>${esc(s.tool)}</code>`).join(' ')}</td>
          <td class="num">${r.status === 'running' ? '…' : fmtMs(r.duration_ms)}</td></tr>`).join('')}
        </tbody></table>`);
    } else {
      const calls = [...p.calls].sort((a, b) => t(b.step.start) - t(a.step.start));
      const d = calls.filter((c) => c.step.status !== 'running').map((c) => c.step.duration_ms);
      const errs = calls.filter((c) => c.step.status === 'error').length;
      const confs = calls.map((c) => c.step.confidence).filter((x) => x != null);
      const flagged = calls.filter((c) => (c.step.flags || []).length);
      const byAgent = {};
      calls.forEach((c) => (byAgent[c.run.agent] = (byAgent[c.run.agent] || 0) + 1));
      openDrawer(`<h3>${esc(n.kind === 'tool' ? n.tool : n.label)}${n.ghost ? ' <span class="muted">(não existe)</span>' : ''}</h3>
        <div class="meta">${kindLbl}${n.kind === 'tool' ? ' · ' + esc(n.server) : ''} · últimos ${WINDOW_MIN} min · ${Object.entries(byAgent).map(([a, c]) => `${esc(a)} ${c}×`).join(' · ') || 'sem chamadas'}</div>`,
        `${n.ghost ? `<div class="flags"><div class="flag hallucination"><span class="st hallucination">!</span><div>O modelo chamou uma tool que <b>não foi oferecida</b> a ele. Toda chamada aqui é alucinação.</div></div></div>` : ''}
        <div class="stats">${[
          stat('Chamadas', calls.length), stat('p50', fmtMs(quant(d, 0.5))), stat('p95', fmtMs(quant(d, 0.95))),
          stat('máx', fmtMs(d.length ? Math.max(...d) : null)), stat('Erros', pct(errs, calls.length)),
          stat('Confiança média', confs.length ? Math.round((100 * confs.reduce((a, b) => a + b, 0)) / confs.length) + '%' : '—'),
          stat('Incertezas', p.unc), stat('Alucinações', p.hal),
        ].join('')}</div>
        <h4>Latência das últimas chamadas</h4>${spark(calls.slice(0, 60).reverse().map((c) => ({ v: c.step.duration_ms, cls: (c.step.flags || []).some((f) => f.level === 'hallucination') ? 'flag' : c.step.status === 'error' ? 'error' : '' })))}
        ${flagged.length ? `<h4>Sinais nesta ${n.kind === 'tool' ? 'tool' : 'origem'}</h4><div class="flags">${flagged.slice(0, 8).flatMap((c) => c.step.flags.map((f) => flagHTML(f, c.run.agent))).join('')}</div>` : ''}
        <h4>Chamadas</h4>
        <table class="calls"><thead><tr><th></th><th>hora</th><th>agente</th>${n.kind === 'server' ? '<th>tool</th>' : ''}<th>motivo</th><th class="num">conf.</th><th class="num">duração</th></tr></thead><tbody>
        ${calls.slice(0, 80).map(({ run, step }) => {
          const w = (step.flags || []).some((f) => f.level === 'hallucination') ? 'hallucination' : (step.flags || []).length ? 'uncertain' : '';
          return `<tr data-run="${esc(run.id)}" data-span="${esc(step.span_id)}"><td>${statusIcon(step.status, w)}</td><td class="num">${hhmmss(t(step.start))}</td>
            <td>${esc(run.agent)}</td>${n.kind === 'server' ? `<td><code>${esc(step.tool)}</code></td>` : ''}
            <td class="why">${esc(step.rationale || '—')}</td><td class="num">${step.confidence != null ? Math.round(step.confidence * 100) + '%' : '—'}</td>
            <td class="num">${step.status === 'running' ? '…' : fmtMs(step.duration_ms)}</td></tr>`;
        }).join('')}
        </tbody></table>`);
    }
    $('drawer-body').querySelectorAll('tr[data-run]').forEach((tr) => tr.addEventListener('click', () => openRun(tr.dataset.run, tr.dataset.span)));
  }
  const stat = (k, v) => `<div class="stat"><div class="k">${k}</div><div class="v">${v}</div></div>`;

  function spark(pts) {
    pts = pts.filter((p) => p.v != null);
    if (!pts.length) return '<p class="muted">sem dados</p>';
    const W = 600, H = 72, max = Math.max(...pts.map((p) => p.v)) || 1, bw = W / Math.max(pts.length, 20);
    const p95 = quant(pts.map((p) => p.v), 0.95);
    const y95 = H - 4 - ((H - 14) * p95) / max;
    return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="latências; p95 ${fmtMs(p95)}">
      ${pts.map((p, i) => { const h = Math.max(2, ((H - 14) * p.v) / max); return `<rect class="bar ${p.cls}" x="${i * bw + 1}" y="${H - 4 - h}" width="${Math.max(1, bw - 2)}" height="${h}" rx="2"><title>${fmtMs(p.v)}</title></rect>`; }).join('')}
      <line class="p95" x1="0" x2="${W}" y1="${y95}" y2="${y95}"/><text x="${W - 4}" y="${y95 - 3}" text-anchor="end">p95 ${fmtMs(p95)}</text></svg>`;
  }

  function flagHTML(f, who) {
    return `<div class="flag ${f.level}"><span class="st ${f.level}">${f.level === 'hallucination' ? '!' : '?'}</span>
      <div><code>${esc(f.code)}</code> — ${esc(f.reason)}</div><span class="src">${who ? esc(who) + ' · ' : ''}${f.source === 'agent' ? 'reportado pelo agente' : 'heurística mirante'}</span></div>`;
  }

  // JSON com destaque de sintaxe
  function prettyJSON(raw) {
    if (raw == null) return null;
    let v = raw;
    if (typeof v === 'string') { const s = v.trim(); if (s.startsWith('{') || s.startsWith('[')) { try { v = JSON.parse(s); } catch { /* texto puro */ } } }
    const txt = typeof v === 'string' ? v : JSON.stringify(v, null, 2);
    if (typeof v === 'string') return { txt, html: esc(txt) };
    const html = esc(txt).replace(/(&quot;(?:\\.|[^&]|&(?!quot;))*?&quot;)(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g, (m, str, colon, bool) => {
      if (str) return colon ? `<span class="j-k">${str}</span>${colon}` : `<span class="j-s">${str}</span>`;
      if (bool) return `<span class="j-b">${m}</span>`;
      return `<span class="j-n">${m}</span>`;
    });
    return { txt, html };
  }
  const payloads = [];
  function payloadBlock(label, raw, open) {
    const p = prettyJSON(raw);
    if (!p) return '';
    const i = payloads.push(p.txt) - 1;
    return `<details class="payload"${open ? ' open' : ''}><summary>${label} <span class="muted">${p.txt.length.toLocaleString('pt-BR')} chars</span><button class="copy" data-copy="${i}">copiar</button></summary><pre class="json">${p.html}</pre></details>`;
  }

  let runFetch = 0;
  async function openRun(id, span) {
    drawer.node = null;
    drawer.run = id;
    nodes.forEach((x) => x.g.classList.remove('sel'));
    const my = ++runFetch;
    let r;
    try {
      const res = await fetch('api/runs/' + encodeURIComponent(id));
      if (!res.ok) throw new Error(res.status === 404 ? 'run saiu da memória do servidor' : 'HTTP ' + res.status);
      r = await res.json();
    } catch (err) {
      openDrawer('<h3>Run indisponível</h3>', `<p class="muted">${esc(err.message)}</p>`);
      return;
    }
    if (my !== runFetch || drawer.run !== id) return;
    renderRun(r, span);
  }

  let refreshT = 0;
  function refreshRunSoon() {
    clearTimeout(refreshT);
    refreshT = setTimeout(async () => {
      if (!drawer.run) return;
      const body = $('drawer-body'), top = body.scrollTop;
      const open = [...body.querySelectorAll('details[open]')].map((d) => d.closest('[data-span]')?.dataset.span + '|' + d.querySelector('summary').firstChild.textContent);
      try {
        const r = await (await fetch('api/runs/' + encodeURIComponent(drawer.run))).json();
        renderRun(r);
        body.querySelectorAll('details').forEach((d) => { if (open.includes(d.closest('[data-span]')?.dataset.span + '|' + d.querySelector('summary').firstChild.textContent)) d.open = true; });
        body.scrollTop = top;
      } catch { /* mantém o que está na tela */ }
    }, 600);
  }

  function renderRun(r, span) {
    payloads.length = 0;
    const w = worst(r);
    const start = t(r.start);
    const end = r.status === 'running' ? Date.now() : start + (r.duration_ms || 0);
    const total = Math.max(1, end - start, ...(r.steps || []).map((s) => t(s.start) - start + (s.duration_ms || 0)));
    const conf = (c) => (c == null ? '—' : `<span class="conf${c < 0.6 ? ' low' : ''}"><span class="track"><span class="fill" style="width:${Math.round(c * 100)}%"></span></span>${Math.round(c * 100)}%</span>`);

    const wf = (r.steps || []).map((s, i) => {
      const off = t(s.start) - start;
      const dur = s.status === 'running' ? Date.now() - t(s.start) : s.duration_ms || 0;
      const hal = (s.flags || []).some((f) => f.level === 'hallucination');
      const cls = `${s.kind}${s.status === 'error' ? ' error' : ''}${s.status === 'running' ? ' running' : ''}${hal ? ' hal' : ''}`;
      const name = s.kind === 'decision' ? `<span class="k">LLM</span> ${s.chosen?.length ? '→ ' + esc(s.chosen.join(', ')) : 'resposta'}` : `<span class="k">${esc(s.server || '')}</span> ${esc(s.tool)}`;
      return `<div class="nm" data-goto="${esc(s.span_id)}">${statusIcon(s.status, (s.flags || []).length ? (hal ? 'hallucination' : 'uncertain') : '')}${name}</div>
        <div class="track" data-goto="${esc(s.span_id)}"><div class="bar ${cls}" style="left:${(100 * off) / total}%;width:${(100 * dur) / total}%" title="${fmtMs(dur)}"></div></div>
        <div class="d">${fmtMs(dur)}</div>`;
    }).join('');

    const steps = (r.steps || []).map((s, i) => {
      const flags = (s.flags || []).map((f) => flagHTML(f)).join('');
      if (s.kind === 'decision') {
        return `<div class="step decision" data-span="${esc(s.span_id)}"><div class="step-head"><span class="k">#${i + 1} decisão do LLM</span>
          <span class="t">${s.chosen?.length ? '→ ' + esc(s.chosen.join(', ')) : 'responder ao usuário'}</span><span class="dur">${fmtMs(s.duration_ms)}</span></div>
          ${flags ? `<div class="flags">${flags}</div>` : ''}
          <div class="kv"><span class="k">modelo</span><span>${esc(s.model || '—')}</span>
          <span class="k">confiança</span><span>${conf(s.confidence)}</span>
          ${s.tokens_in || s.tokens_out ? `<span class="k">tokens</span><span>${s.tokens_in || 0} entrada · ${s.tokens_out || 0} saída</span>` : ''}
          <span class="k">raciocínio</span><span>${esc(s.reasoning || '—')}</span>
          ${s.alternatives?.length ? `<span class="k">descartadas</span><div class="alts">${s.alternatives.map((a) => `<div class="alt"><code>${esc(a.tool)}</code>${a.score != null ? ` · ${Math.round(a.score * 100)}%` : ''}${a.why ? ' — ' + esc(a.why) : ''}</div>`).join('')}</div>` : ''}
          </div></div>`;
      }
      return `<div class="step tool${s.status === 'error' ? ' error' : ''}" data-span="${esc(s.span_id)}"><div class="step-head"><span class="k">#${i + 1} ${esc(s.server || 'tool')}</span>
        <span class="t">${esc(s.tool)}</span>${statusIcon(s.status, '')}<span class="dur">${s.status === 'running' ? `<span data-live-start="${t(s.start)}"></span>` : fmtMs(s.duration_ms)}</span></div>
        ${flags ? `<div class="flags">${flags}</div>` : ''}
        <div class="kv"><span class="k">por quê</span><span>${esc(s.rationale || '—')}</span>
        <span class="k">confiança</span><span>${conf(s.confidence)}</span>
        <span class="k">início</span><span>${new Date(t(s.start)).toLocaleTimeString('pt-BR')}.${String(t(s.start) % 1000).padStart(3, '0')} (+${fmtMs(t(s.start) - start)})</span>
        ${s.error ? `<span class="k">erro</span><span class="err-txt">${esc(s.error)}</span>` : ''}</div>
        ${payloadBlock('payload enviado (args)', s.args, true)}${payloadBlock('retorno', s.result, false)}</div>`;
    }).join('');

    openDrawer(`<h3>${statusIcon(r.status, w)} ${esc(r.agent)} <span class="muted" style="font-weight:400;font-size:.9rem">${esc(r.id)}</span></h3>
      <div class="meta">${r.user ? esc(r.user) + ' · ' : ''}${new Date(start).toLocaleString('pt-BR')} · ${r.status === 'running' ? 'em andamento' : fmtMs(r.duration_ms)} · ${(r.steps || []).filter((s) => s.kind === 'tool').length} tools · ${(r.steps || []).filter((s) => s.kind === 'decision').length} decisões</div>`,
      `<div class="io"><div class="lbl">pergunta</div><div class="txt">${esc(r.input || '—')}</div>
       ${r.output || r.error ? `<div class="lbl">resposta</div><div class="txt">${esc(r.output || '')}${r.error ? `<div class="err-txt">${esc(r.error)}</div>` : ''}</div>` : ''}</div>
       ${(r.flags || []).length ? `<div class="flags">${r.flags.map((f) => flagHTML(f)).join('')}</div>` : ''}
       <h4>Linha do tempo</h4><div class="wf">${wf || '<span class="muted">sem passos</span>'}</div>
       <h4>Passo a passo</h4>${steps}
       ${payloadBlock('run completo (JSON)', JSON.stringify(r), false)}`);

    const body = $('drawer-body');
    body.querySelectorAll('[data-goto]').forEach((el) => el.addEventListener('click', () => goto(el.dataset.goto)));
    if (span) goto(span);
  }
  function goto(span) {
    const el = $('drawer-body').querySelector(`.step[data-span="${CSS.escape(span)}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    el.classList.remove('flash'); void el.offsetWidth; el.classList.add('flash');
  }
  $('drawer-body').addEventListener('click', async (ev) => {
    const b = ev.target.closest('.copy');
    if (!b) return;
    ev.preventDefault();
    const txt = payloads[+b.dataset.copy];
    try { await navigator.clipboard.writeText(txt); }
    catch {
      const ta = Object.assign(document.createElement('textarea'), { value: txt });
      document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove();
    }
    b.textContent = 'copiado ✓';
    setTimeout(() => (b.textContent = 'copiar'), 1500);
  });

  // ---------- conectar agente (gera as URLs do modo proxy) ----------
  async function openConnect() {
    drawer.run = drawer.node = null;
    let up = { llm: [], mcp: [] };
    try { up = await (await fetch('api/upstreams')).json(); } catch { /* sem proxy */ }
    const base = location.origin;
    const render = (agent) => {
      payloads.length = 0;
      const a = encodeURIComponent(agent || 'meu-agente');
      const row = (k, url, hint) => {
        const i = payloads.push(url) - 1;
        return `<div class="cx-row"><span class="k">${esc(k)}</span><div><code>${esc(url)}</code>${hint ? `<div class="muted" style="font-size:.75rem">${esc(hint)}</div>` : ''}</div><button class="copy" data-copy="${i}">copiar</button></div>`;
      };
      const llm = up.llm.map((n) => row(`LLM · ${n}`, `${base}/p/${a}/llm/${n}`, 'troque a URL base do LLM por esta (Ollama: /api/chat · OpenAI: /v1/chat/completions)')).join('');
      const mcp = up.mcp.map((n) => row(`MCP · ${n}`, `${base}/p/${a}/mcp/${n}`, 'troque o endpoint do MCP server por este')).join('');
      const ev = row('Eventos (SDK)', `${base}/v1/events`, 'só se for instrumentar por código');
      $('cx-urls').innerHTML = (llm || mcp)
        ? `<h4>Modo proxy — zero código</h4>${llm}${mcp}<h4>Modo SDK</h4>${ev}`
        : `<p class="cx-note">Este mirante ainda não tem upstreams de proxy. Suba com <code>--llm nome=URL</code> e/ou <code>--mcp server=URL</code> (ou <code>MIRANTE_LLM</code>/<code>MIRANTE_MCP</code>).</p><h4>Modo SDK</h4>${ev}`;
    };
    openDrawer('<h3>Conectar agente</h3><div class="meta">o agente só troca URLs — nenhuma linha de código</div>',
      `<div class="cx-field"><label for="cx-agent">Nome do agente (como vai aparecer no mapa)</label><input id="cx-agent" value="meu-agente" autocomplete="off" spellcheck="false"></div>
       <div id="cx-urls"></div>
       <h4>Como funciona</h4>
       <p class="cx-note">O mirante encaminha cada requisição para o upstream original sem alterar nada${up.inject_reasoning ? ' (exceto: injeta <code>reason</code>/<code>confidence</code> no schema das tools e remove da resposta, para registrar o porquê)' : ''} e observa no caminho: pergunta, tools oferecidas, decisão do modelo, argumentos, retorno, tempos e resposta final. Se usar só a URL do MCP, cada sessão de chamadas vira um run sem a visão do LLM.</p>`);
    const inp = $('cx-agent');
    inp.addEventListener('input', () => render(inp.value.trim().replace(/[^\w.-]+/g, '-')));
    render(inp.value);
    inp.select();
  }
  $('connect-btn').addEventListener('click', openConnect);

  // ---------- telão: teclado, cursor, auto-fechar ----------
  let idleT = 0, lastAct = Date.now();
  const activity = () => {
    lastAct = Date.now();
    document.body.classList.remove('idle');
    clearTimeout(idleT);
    idleT = setTimeout(() => document.body.classList.add('idle'), 3000);
  };
  ['mousemove', 'keydown', 'click', 'wheel', 'touchstart'].forEach((e) => addEventListener(e, activity, { passive: true }));
  activity();
  setInterval(() => { if (drawer.el.classList.contains('open') && Date.now() - lastAct > 120e3) closeDrawer(); }, 5000);
  addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') closeDrawer();
    if ((ev.key === 'f' || ev.key === 'F') && !ev.ctrlKey && !ev.metaKey) {
      if (document.fullscreenElement) document.exitFullscreen(); else document.documentElement.requestFullscreen?.();
    }
  });
  let rz = 0;
  addEventListener('resize', () => { clearTimeout(rz); rz = setTimeout(() => layout(false), 120); });

  // ---------- conexão ----------
  function connect() {
    const es = new EventSource('api/stream');
    const live = $('live');
    es.addEventListener('snapshot', (m) => { live.classList.add('on'); $('live-txt').textContent = 'ao vivo'; onSnapshot(JSON.parse(m.data)); });
    es.addEventListener('update', (m) => onUpdate(JSON.parse(m.data)));
    es.onerror = () => { live.classList.remove('on'); $('live-txt').textContent = 'reconectando…'; };
  }
  connect();
})();
