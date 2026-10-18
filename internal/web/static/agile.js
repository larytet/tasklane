// Boards, backlog, sprints and reports. Plugs into the app shell through window.TLApp (see app.js).
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, S, avatar, lozenge, PRI } = A;
  const NS = 'http://www.w3.org/2000/svg';

  const day = iso => new Date(iso).toISOString().slice(0, 10);
  const num = n => (Math.round(n * 100) / 100).toString();
  const errText = r => (r.data && r.data.error) || 'Something went wrong';

  function subnav(board, active) {
    const link = (k, label) => h('a', { href: '#/' + k + '/' + board.id, class: 'tab', 'aria-current': active === k ? 'page' : null }, label);
    return h('nav', { class: 'tabs', 'aria-label': 'Board views' },
      h('span', { class: 'tabs-title' }, board.project + ' · ' + board.name),
      link('board', 'Board'), board.kind === 'scrum' ? link('backlog', 'Backlog') : null, link('reports', 'Reports'));
  }

  // ---------- boards list ----------
  async function boardsView(project) {
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === project }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/boards/' + sel.value; });
    const list = h('div', { class: 'card', role: 'list' });
    clear(document.getElementById('app')).append(topbar(), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Boards'), sel,
      h('button', { class: 'btn', onclick: () => createBoardDialog(project) }, 'Create board')), list));
    const r = await api('GET', '/api/v1/projects/' + project + '/boards');
    if (r.status !== 200) { list.append(h('div', { class: 'row' }, 'Boards could not be loaded.')); return; }
    if (!r.data.length) list.append(h('div', { class: 'row' }, h('span', { class: 'muted' }, 'This project has no boards yet.')));
    for (const b of r.data) {
      list.append(h('div', { class: 'row', role: 'listitem', tabindex: 0, onclick: () => { location.hash = '#/board/' + b.id; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/board/' + b.id; } },
        h('span', { class: 'sum' }, b.name), h('span', { class: 'loz todo' }, b.kind === 'scrum' ? 'Scrum' : 'Kanban')));
    }
  }

  function createBoardDialog(project) {
    const name = h('input', { type: 'text', 'aria-label': 'Board name', placeholder: 'Board name', style: 'width:100%' });
    const kind = h('select', { 'aria-label': 'Board type' }, h('option', { value: 'scrum' }, 'Scrum (sprints)'), h('option', { value: 'kanban' }, 'Kanban (continuous flow)'));
    const msg = h('div', { class: 'err', role: 'alert' });
    const close = modal(h('div', {}, h('h2', {}, 'Create board'), name, h('div', { style: 'height:8px' }), kind, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
        h('button', { class: 'btn primary', onclick: async () => {
          const r = await api('POST', '/api/v1/projects/' + project + '/boards', { name: name.value, kind: kind.value });
          if (r.status === 201) { close(); location.hash = '#/board/' + r.data.id; } else msg.textContent = errText(r);
        } }, 'Create'))));
  }

  // ---------- board ----------
  function card(c, cols, onMove, dragState) {
    const sel = h('select', { class: 'move', 'aria-label': 'Move ' + c.key + ' to column' }, h('option', { value: '' }, 'Move to…'), cols.map(x => h('option', { value: x.id }, x.name)));
    sel.addEventListener('change', () => { if (sel.value) onMove(c.key, sel.value, {}); sel.value = ''; });
    const el = h('div', { class: 'bcard', role: 'listitem', draggable: 'true', 'data-key': c.key, tabindex: 0, 'aria-label': c.key + ' ' + c.summary },
      h('a', { href: '#/issue/' + c.key, class: 'bsum' }, c.summary),
      c.labels.length ? h('div', { class: 'chips' }, c.labels.map(l => h('span', { class: 'label' }, l))) : null,
      h('div', { class: 'bfoot' },
        h('span', { class: 'tile ' + c.type, title: c.type }), h('span', { class: 'key' }, c.key),
        h('span', { class: 'pri ' + c.priority, title: c.priority + ' priority' }, PRI[c.priority] || ''),
        c.estimate != null ? h('span', { class: 'est', title: 'Estimate' }, num(c.estimate)) : null,
        c.subtasks ? h('span', { class: 'muted', title: 'Subtasks done' }, c.subtasks_done + '/' + c.subtasks) : null,
        c.carried_over ? h('span', { class: 'badge warn', title: 'Left unfinished in earlier sprints' }, 'carried over ' + c.carried_over + '×') : null,
        c.needs_input ? h('span', { class: 'badge info' }, 'needs input') : null,
        h('span', { class: 'grow' }), c.assignee ? avatar(c.assignee) : null),
      sel);
    el.addEventListener('dragstart', e => { dragState.key = c.key; e.dataTransfer.setData('text/plain', c.key); e.dataTransfer.effectAllowed = 'move'; el.classList.add('dragging'); });
    el.addEventListener('dragend', () => { dragState.key = null; el.classList.remove('dragging'); });
    return el;
  }

  // the card the pointer is above decides where a dropped card lands: before it, or at the end
  function dropTarget(body, y, dragging) {
    for (const c of body.querySelectorAll('.bcard')) {
      if (c.dataset.key === dragging) continue;
      const r = c.getBoundingClientRect();
      if (y < r.top + r.height / 2) return c.dataset.key;
    }
    return '';
  }

  async function boardView(id) {
    const state = { quick: new Set(), drag: { key: null }, busy: false };
    const root = h('div', { class: 'page wide' });
    clear(document.getElementById('app')).append(topbar(), root);
    async function load() {
      const q = [...state.quick].join(',');
      const r = await api('GET', '/api/v1/boards/' + id + (q ? '?quick=' + encodeURIComponent(q) : ''));
      clear(root);
      if (r.status !== 200) { root.append(h('div', { class: 'banner danger', role: 'alert' }, r.status === 404 ? 'This board does not exist, or you cannot see it.' : errText(r))); return; }
      const v = r.data, b = v.board;
      root.append(subnav(b, 'board'));
      if (b.quick_filters.length) {
        root.append(h('div', { class: 'chips', role: 'group', 'aria-label': 'Quick filters' }, b.quick_filters.map(f => {
          const on = state.quick.has(f.name);
          return h('button', { class: 'chip-btn' + (on ? ' on' : ''), 'aria-pressed': on ? 'true' : 'false', onclick: () => { if (on) state.quick.delete(f.name); else state.quick.add(f.name); load(); } }, f.name);
        })));
      }
      if (b.kind === 'scrum' && v.columns.every(c => !c.cards.length) && !v.unmapped.length) {
        root.append(h('p', { class: 'muted' }, 'No active sprint has issues. Plan and start a sprint on the ', h('a', { href: '#/backlog/' + b.id }, 'Backlog'), '.'));
      }
      const cols = v.columns;
      const move = async (key, column, pos) => {
        state.busy = true;
        const rr = await api('POST', '/api/v1/boards/' + id + '/move', Object.assign({ key, column }, pos));
        state.busy = false;
        if (rr.status !== 200) toast(errText(rr));
        else if (rr.data.transitioned) toast(key + ' moved by "' + rr.data.transition + '".');
        load();
      };
      const lanes = v.lanes && v.lanes.length ? v.lanes : [{ name: '', keys: null }];
      for (const lane of lanes) {
        const sec = h('section', { class: 'lane', 'aria-label': lane.name ? 'Swimlane ' + lane.name : 'Board' });
        if (lane.name) sec.append(h('h3', { class: 'lane-name' }, lane.name));
        const row = h('div', { class: 'board' });
        for (const col of cols) {
          const cards = lane.keys ? col.cards.filter(c => lane.keys.includes(c.key)) : col.cards;
          const body = h('div', { class: 'col-body', role: 'list', 'aria-label': col.name + ' cards' }, cards.map(c => card(c, cols, move, state.drag)));
          body.addEventListener('dragover', e => { e.preventDefault(); body.classList.add('over'); });
          body.addEventListener('dragleave', () => body.classList.remove('over'));
          body.addEventListener('drop', e => {
            e.preventDefault(); body.classList.remove('over');
            const key = state.drag.key || e.dataTransfer.getData('text/plain');
            if (!key) return;
            const before = dropTarget(body, e.clientY, key);
            move(key, col.id, before ? { before } : {});
          });
          const wip = col.max_wip != null || col.min_wip != null
            ? h('span', { class: 'wip ' + (col.breach || '') }, col.count + (col.max_wip != null ? ' / ' + col.max_wip : '') + (col.breach === 'over' ? ' over the limit' : col.breach === 'under' ? ' under the minimum' : ''))
            : h('span', { class: 'muted' }, String(col.count));
          row.append(h('section', { class: 'col' + (col.breach ? ' breach-' + col.breach : ''), 'aria-label': col.name }, h('header', {}, h('h3', {}, col.name), wip), body));
        }
        sec.append(row);
        root.append(sec);
      }
      if (v.unmapped.length) root.append(h('p', { class: 'muted' }, 'On no column: ' + v.unmapped.map(c => c.key + ' (' + c.status + ')').join(', ')));
    }
    await load();
    A.timer(() => { if (!document.hidden && !state.busy && !state.drag.key) load(); }, 20000);
  }

  // ---------- backlog and sprints ----------
  async function backlogView(id) {
    const root = h('div', { class: 'page wide' });
    clear(document.getElementById('app')).append(topbar(), root);
    const drag = { key: null };
    async function load() {
      const r = await api('GET', '/api/v1/boards/' + id + '/backlog');
      clear(root);
      if (r.status !== 200) { root.append(h('div', { class: 'banner danger', role: 'alert' }, r.status === 400 ? 'A kanban board has no backlog; use the board.' : r.status === 404 ? 'This board does not exist, or you cannot see it.' : errText(r))); return; }
      const v = r.data, b = v.board, admin = v.role === 'admin', canPlan = v.role !== 'viewer';
      root.append(subnav(b, 'backlog'));
      const targets = [{ id: '', name: 'Backlog' }].concat(v.sprints.map(s => ({ id: s.id, name: s.name })));
      const plan = async (keys, sprint, pos) => {
        const rr = await api('POST', '/api/v1/boards/' + id + '/plan', Object.assign({ sprint, keys }, pos || {}));
        if (rr.status !== 200) toast(errText(rr));
        load();
      };
      const rows = (cards, sprintId) => cards.map(c => {
        const sel = h('select', { class: 'move', 'aria-label': 'Move ' + c.key + ' to' }, h('option', { value: '__' }, 'Move to…'), targets.filter(t => t.id !== sprintId).map(t => h('option', { value: t.id }, t.name)));
        sel.addEventListener('change', () => { if (sel.value !== '__') plan([c.key], sel.value); });
        const row = h('div', { class: 'row brow', role: 'listitem', draggable: canPlan ? 'true' : null, 'data-key': c.key },
          h('span', { class: 'tile ' + c.type }), h('a', { class: 'key', href: '#/issue/' + c.key }, c.key), h('span', { class: 'sum' }, c.summary),
          c.carried_over ? h('span', { class: 'badge warn' }, 'carried over ' + c.carried_over + '×') : null,
          lozenge(c.status, c.category), c.estimate != null ? h('span', { class: 'est' }, num(c.estimate)) : null,
          c.assignee ? avatar(c.assignee) : null, canPlan ? sel : null);
        row.addEventListener('dragstart', e => { drag.key = c.key; e.dataTransfer.setData('text/plain', c.key); e.dataTransfer.effectAllowed = 'move'; });
        row.addEventListener('dragend', () => { drag.key = null; });
        return row;
      });
      const panel = (title, sub, cards, sprint) => {
        const sprintId = sprint ? sprint.id : '';
        const list = h('div', { class: 'card panel-list', role: 'list', 'aria-label': title + ' issues' }, cards.length ? rows(cards, sprintId) : h('div', { class: 'row' }, h('span', { class: 'muted' }, sprint ? 'Plan a sprint by adding issues from the backlog.' : 'The backlog is empty.')));
        list.addEventListener('dragover', e => { if (canPlan) { e.preventDefault(); list.classList.add('over'); } });
        list.addEventListener('dragleave', () => list.classList.remove('over'));
        list.addEventListener('drop', e => {
          e.preventDefault(); list.classList.remove('over');
          const key = drag.key || e.dataTransfer.getData('text/plain');
          if (key) plan([key], sprintId, {});
        });
        const quick = h('input', { type: 'text', placeholder: 'Create an issue in ' + title, 'aria-label': 'Create an issue in ' + title, class: 'grow' });
        const add = async () => {
          if (!quick.value.trim()) return;
          const rr = await api('POST', '/api/v1/issues', { project: b.project, type: 'Task', summary: quick.value.trim() });
          if (rr.status !== 201) { toast(errText(rr)); return; }
          if (sprintId) await plan([rr.data.key], sprintId, {}); else load();
        };
        quick.addEventListener('keydown', e => { if (e.key === 'Enter') add(); });
        return h('section', { class: 'sprint', 'aria-label': title }, sub, list, canPlan ? h('div', { class: 'chips' }, quick, h('button', { class: 'btn', onclick: add }, 'Create')) : null);
      };
      for (const s of v.sprints) {
        const t = s.totals, pct = s.capacity_hours > 0 ? Math.min(100, Math.round(100 * s.committed_estimate / s.capacity_hours)) : 0;
        const head = h('header', { class: 'sprint-head' },
          h('div', { class: 'sprint-title' }, h('h2', {}, s.name), h('span', { class: 'muted' }, day(s.starts_at) + ' to ' + day(new Date(new Date(s.ends_at) - 1000)) + ' · ' + s.working_days + ' working days' + (s.state === 'active' ? ' · ' + s.days_left + ' left' : '')),
            h('span', { class: 'loz ' + (s.state === 'active' ? 'in_progress' : 'todo') }, s.state === 'active' ? 'Active' : 'Future')),
          s.goal ? h('div', { class: 'muted' }, 'Goal: ' + s.goal) : null,
          h('div', { class: 'chips' },
            h('span', { class: 'loz todo' }, 'To Do ' + t.todo.count + ' (' + num(t.todo.estimate) + ')'),
            h('span', { class: 'loz in_progress' }, 'In Progress ' + t.in_progress.count + ' (' + num(t.in_progress.estimate) + ')'),
            h('span', { class: 'loz done' }, 'Done ' + t.done.count + ' (' + num(t.done.estimate) + ')'),
            s.scope_added || s.scope_removed ? h('span', { class: 'muted' }, 'scope: +' + s.scope_added + ' −' + s.scope_removed) : null,
            h('span', { class: 'grow' }),
            admin && s.state === 'future' ? h('button', { class: 'btn primary', onclick: async () => { const rr = await api('POST', '/api/v1/sprints/' + s.id + '/start'); if (rr.status !== 200) toast(errText(rr)); load(); } }, 'Start sprint') : null,
            admin && s.state === 'active' ? h('button', { class: 'btn primary', onclick: () => completeDialog(s, load) }, 'Complete sprint') : null),
          s.capacity_hours > 0 ? h('div', { class: 'cap', role: 'img', 'aria-label': 'Capacity ' + num(s.capacity_hours) + ' hours; committed estimate ' + num(s.committed_estimate) + (s.commit ? ' (' + s.commit + ')' : '') },
            h('div', { class: 'cap-fill' + (s.commit === 'over' ? ' over' : ''), style: 'width:' + pct + '%' })) : null);
        root.append(panel(s.name, head, s.issues, s));
      }
      root.append(panel('Backlog', h('header', { class: 'sprint-head' }, h('h2', {}, 'Backlog'),
        h('span', { class: 'muted' }, v.backlog.length + ' issues · estimate ' + num(v.backlog_totals.todo.estimate + v.backlog_totals.in_progress.estimate))), v.backlog, null));
    }
    await load();
  }

  function completeDialog(s, done) {
    const dest = h('select', { 'aria-label': 'Move unfinished issues to' }, h('option', { value: 'next' }, 'The next sprint'), h('option', { value: 'new' }, 'A new sprint'), h('option', { value: 'backlog' }, 'The backlog'));
    const out = h('div', { 'aria-live': 'polite', class: 'muted' }), msg = h('div', { class: 'err', role: 'alert' });
    const apply = h('button', { class: 'btn primary', disabled: true }, 'Complete sprint');
    async function preview() {
      apply.disabled = true;
      const r = await api('POST', '/api/v1/sprints/' + s.id + '/complete', { destination: dest.value, dry_run: true });
      if (r.status !== 200) { clear(out); msg.textContent = errText(r); return; }
      msg.textContent = '';
      clear(out).append('Done: ' + r.data.done.length + '. Unfinished: ' + r.data.moved.length + ' → ' + r.data.destination + (r.data.moved.length ? ' (' + r.data.moved.join(', ') + ')' : '') + '.');
      apply.disabled = false;
    }
    dest.addEventListener('change', preview);
    const close = modal(h('div', {}, h('h2', {}, 'Complete ' + s.name), h('label', {}, 'Move unfinished issues to ', dest), h('div', { style: 'height:8px' }), out, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), apply)));
    apply.addEventListener('click', async () => {
      const r = await api('POST', '/api/v1/sprints/' + s.id + '/complete', { destination: dest.value });
      if (r.status === 200) { close(); toast(s.name + ' completed.'); done(); } else msg.textContent = errText(r);
    });
    preview();
  }

  // ---------- reports ----------
  function svg(tag, attrs, ...kids) {
    const e = document.createElementNS(NS, tag);
    for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
    for (const c of kids.flat()) if (c) e.append(typeof c === 'string' ? document.createTextNode(c) : c);
    return e;
  }

  // A small line chart with a table of the same numbers below it, so the data is available without seeing the picture.
  function lineChart(title, xs, series) {
    const W = 640, H = 240, P = 36;
    const max = Math.max(1, ...series.flatMap(s => s.values));
    const x = i => P + (xs.length < 2 ? 0 : i * (W - 2 * P) / (xs.length - 1));
    const y = v => H - P - v * (H - 2 * P) / max;
    const g = svg('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': title, class: 'chart' }, svg('title', {}, title),
      svg('line', { x1: P, y1: H - P, x2: W - P, y2: H - P, class: 'axis' }), svg('line', { x1: P, y1: P / 2, x2: P, y2: H - P, class: 'axis' }),
      svg('text', { x: 4, y: P / 2 + 4, class: 'tick' }, num(max)), svg('text', { x: 4, y: H - P, class: 'tick' }, '0'),
      xs.length ? svg('text', { x: P, y: H - 8, class: 'tick' }, xs[0]) : null, xs.length > 1 ? svg('text', { x: W - P, y: H - 8, class: 'tick', 'text-anchor': 'end' }, xs[xs.length - 1]) : null,
      series.map((s, k) => svg('polyline', { points: s.values.map((v, i) => x(i) + ',' + y(v)).join(' '), class: 'line s' + k, fill: 'none' })));
    const legend = h('div', { class: 'chips' }, series.map((s, k) => h('span', { class: 'legend s' + k }, s.label)));
    const table = h('details', {}, h('summary', {}, 'Data table'), h('table', { class: 'data' },
      h('thead', {}, h('tr', {}, h('th', {}, 'When'), series.map(s => h('th', {}, s.label)))),
      h('tbody', {}, xs.map((t, i) => h('tr', {}, h('td', {}, t), series.map(s => h('td', {}, num(s.values[i]))))))));
    return h('figure', { class: 'fig' }, h('figcaption', {}, title), g, legend, table);
  }

  function barChart(title, labels, series) {
    const W = 640, H = 240, P = 36, n = labels.length;
    const max = Math.max(1, ...series.flatMap(s => s.values));
    const bw = n ? (W - 2 * P) / n : 0, sw = bw / (series.length + 1);
    const g = svg('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': title, class: 'chart' }, svg('title', {}, title),
      svg('line', { x1: P, y1: H - P, x2: W - P, y2: H - P, class: 'axis' }), svg('text', { x: 4, y: P / 2 + 4, class: 'tick' }, num(max)),
      series.map((s, k) => s.values.map((v, i) => svg('rect', { x: P + i * bw + (k + .5) * sw, y: H - P - v * (H - 2 * P) / max, width: sw * .9, height: v * (H - 2 * P) / max, class: 'bar s' + k }))),
      labels.map((l, i) => svg('text', { x: P + i * bw + bw / 2, y: H - 12, class: 'tick', 'text-anchor': 'middle' }, l.length > 12 ? l.slice(0, 11) + '…' : l)));
    const legend = h('div', { class: 'chips' }, series.map((s, k) => h('span', { class: 'legend s' + k }, s.label)));
    const table = h('details', {}, h('summary', {}, 'Data table'), h('table', { class: 'data' }, h('thead', {}, h('tr', {}, h('th', {}, 'Item'), series.map(s => h('th', {}, s.label)))),
      h('tbody', {}, labels.map((l, i) => h('tr', {}, h('td', {}, l), series.map(s => h('td', {}, num(s.values[i]))))))));
    return h('figure', { class: 'fig' }, h('figcaption', {}, title), g, legend, table);
  }

  const simpleTable = (cols, rows) => h('table', { class: 'data' }, h('thead', {}, h('tr', {}, cols.map(c => h('th', {}, c[0])))), h('tbody', {}, rows.map(r => h('tr', {}, cols.map(c => h('td', {}, String(r[c[1]] == null ? '' : r[c[1]])))))));

  async function reportsView(id, kind) {
    const root = h('div', { class: 'page wide' });
    clear(document.getElementById('app')).append(topbar(), root);
    const br = await api('GET', '/api/v1/boards/' + id);
    if (br.status !== 200) { root.append(h('div', { class: 'banner danger', role: 'alert' }, 'This board does not exist, or you cannot see it.')); return; }
    const b = br.data.board;
    const kinds = [['burndown', 'Burndown', true], ['burnup', 'Burnup', true], ['sprint', 'Sprint report', true], ['velocity', 'Velocity', true], ['flow', 'Cumulative flow'], ['control', 'Control chart'],
      ['created', 'Created vs resolved'], ['workload', 'Workload'], ['status-time', 'Time in status']].filter(k => b.kind === 'scrum' || !k[2]);
    kind = kind || kinds[0][0];
    const sel = h('select', { 'aria-label': 'Report' }, kinds.map(k => h('option', { value: k[0], selected: k[0] === kind }, k[1])));
    sel.addEventListener('change', () => { location.hash = '#/reports/' + id + '/' + sel.value; });
    const days = h('select', { 'aria-label': 'Period' }, [7, 14, 30, 90].map(d => h('option', { value: d, selected: d === 30 }, 'Last ' + d + ' days')));
    const body = h('div', { 'aria-live': 'polite' });
    let sprintSel = null;
    root.append(subnav(b, 'reports'), h('div', { class: 'chips' }, sel, days), body);
    const draw = async () => {
      clear(body);
      const q = '?days=' + days.value;
      if (['burndown', 'burnup', 'sprint'].includes(kind)) {
        const sr = await api('GET', '/api/v1/boards/' + id + '/sprints');
        const list = (sr.data || []).filter(s => s.state !== 'future');
        if (!list.length) { body.append(h('p', { class: 'muted' }, 'No sprint has started yet.')); return; }
        if (!sprintSel) { sprintSel = h('select', { 'aria-label': 'Sprint' }, list.map(s => h('option', { value: s.id }, s.name + (s.state === 'active' ? ' (active)' : '')))); sprintSel.addEventListener('change', draw); days.after(sprintSel); }
        const r = await api('GET', '/api/v1/sprints/' + sprintSel.value + '/report');
        if (r.status !== 200) { body.append(h('div', { class: 'err' }, errText(r))); return; }
        const rep = r.data, xs = rep.burn.map(p => p.at.slice(0, 16).replace('T', ' '));
        if (kind === 'burndown') body.append(lineChart('Burndown: ' + rep.sprint.name, xs, [{ label: 'Remaining', values: rep.burn.map(p => p.remaining) }, { label: 'Ideal', values: rep.burn.map(p => p.ideal) }]));
        else if (kind === 'burnup') body.append(lineChart('Burnup: ' + rep.sprint.name, xs, [{ label: 'Scope', values: rep.burn.map(p => p.scope) }, { label: 'Done', values: rep.burn.map(p => p.done) }]));
        else {
          const sec = (label, items) => h('section', {}, h('h3', {}, label + ' (' + items.length + ')'), items.length ? simpleTable([['Key', 'key'], ['Summary', 'summary'], ['Estimate', 'estimate']], items) : h('p', { class: 'muted' }, 'None.'));
          body.append(h('p', {}, 'Committed ' + num(rep.committed) + ' · completed ' + num(rep.completed)), sec('Completed', rep.completed_issues), sec('Not completed', rep.not_completed_issues), sec('Removed', rep.removed_issues), sec('Added after start', rep.added_issues));
        }
        return;
      }
      if (kind === 'velocity') {
        const r = await api('GET', '/api/v1/boards/' + id + '/reports/velocity');
        if (!r.data.length) { body.append(h('p', { class: 'muted' }, 'No sprint has been completed yet.')); return; }
        body.append(barChart('Velocity', r.data.map(x => x.sprint), [{ label: 'Committed', values: r.data.map(x => x.committed) }, { label: 'Completed', values: r.data.map(x => x.completed) }]));
        return;
      }
      const path = { flow: 'cumulative-flow', control: 'control-chart', created: 'created-vs-resolved', workload: 'workload', 'status-time': 'time-in-status' }[kind];
      const r = await api('GET', '/api/v1/boards/' + id + '/reports/' + path + q);
      if (r.status !== 200) { body.append(h('div', { class: 'err', role: 'alert' }, errText(r))); return; }
      const d = r.data;
      if (kind === 'flow') body.append(lineChart('Cumulative flow', d.days.map(x => x.day), d.statuses.map(s => ({ label: s, values: d.days.map(x => x.counts[s] || 0) }))));
      else if (kind === 'control') body.append(h('p', {}, 'Average cycle time ' + num(d.average_days) + ' days, standard deviation ' + num(d.std_dev_days)), d.points.length ? simpleTable([['Issue', 'key'], ['Resolved', 'resolved'], ['Cycle days', 'cycle_days']], d.points) : h('p', { class: 'muted' }, 'No issues were resolved in this period.'));
      else if (kind === 'created') body.append(lineChart('Created vs resolved (cumulative)', d.map(x => x.Day), [{ label: 'Created', values: d.map(x => x.CumCreated) }, { label: 'Resolved', values: d.map(x => x.CumResol) }]));
      else if (kind === 'workload') body.append(d.length ? simpleTable([['Assignee', 'assignee'], ['Open issues', 'open'], ['Estimate', 'estimate']], d) : h('p', { class: 'muted' }, 'Nothing is open.'));
      else body.append(simpleTable([['Status', 'status'], ['Issues', 'issues'], ['Total days', 'total_days'], ['Average days per issue', 'avg_days_per_issue']], d));
    };
    days.addEventListener('change', draw);
    await draw();
  }

  A.EXT.routes.push(
    { re: /^#\/boards\/([A-Za-z0-9]*)/, run: m => boardsView(m[1].toUpperCase() || (S.projects[0] && S.projects[0].key) || '') },
    { re: /^#\/board\/([0-9a-f-]{36})/, run: m => boardView(m[1]) },
    { re: /^#\/backlog\/([0-9a-f-]{36})/, run: m => backlogView(m[1]) },
    { re: /^#\/reports\/([0-9a-f-]{36})(?:\/([a-z-]+))?/, run: m => reportsView(m[1], m[2]) });
})();
