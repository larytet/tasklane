// Dashboards: a list, and a page of gadgets (each over a query or a saved filter), computed for whoever is looking.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const KIND = {
    filter_results: 'Filter results', assigned_to_me: 'Assigned to me', activity_stream: 'Activity stream', pie: 'Pie (count by)', stats_2d: 'Two-dimensional statistics',
    created_vs_resolved: 'Created vs resolved', average_age: 'Average age of open issues', workload: 'Workload', sprint_burndown: 'Sprint burndown', text: 'Text', iframe: 'Embedded page',
  };
  const REFRESH = [[0, 'Off'], [30, 'Every 30 seconds'], [60, 'Every minute'], [300, 'Every 5 minutes'], [900, 'Every 15 minutes']];
  const pct = (n, t) => t ? Math.round(1000 * n / t) / 10 : 0;

  // ---------- the list ----------
  async function listView() {
    const app = clear(document.getElementById('app'));
    const list = h('div', { class: 'list', role: 'list', 'aria-label': 'Dashboards' });
    const msg = h('div', { class: 'err', role: 'alert' });
    const name = h('input', { type: 'text', 'aria-label': 'Dashboard name', placeholder: 'Team overview', maxlength: 100 });
    const cols = h('select', { 'aria-label': 'Columns' }, ...[1, 2, 3].map(n => h('option', { value: n }, n + (n === 1 ? ' column' : ' columns'))));
    cols.value = 2;
    const shared = h('input', { type: 'checkbox', id: 'dash-shared' });
    async function load() {
      clear(list);
      const r = await api('GET', '/api/v1/dashboards');
      if (r.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No dashboards yet. Make one below.')));
      for (const d of r.data) {
        list.append(h('div', { class: 'row', role: 'listitem', tabindex: 0, onclick: () => { location.hash = '#/dashboards/' + d.id; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/dashboards/' + d.id; } },
          h('span', { class: 'sum' }, h('strong', {}, d.name), ' ', h('span', { class: 'muted' }, (d.mine ? 'yours' : 'by ' + d.owner) + (d.shared === 'org' ? ' · shared with the organization' : '') + (d.default ? ' · opens by default' : ''))),
          h('button', { class: 'btn ghost', 'aria-pressed': d.favourite ? 'true' : 'false', 'aria-label': (d.favourite ? 'Remove from favourites: ' : 'Favourite: ') + d.name, onclick: async e => { e.stopPropagation(); await api(d.favourite ? 'DELETE' : 'PUT', '/api/v1/dashboards/' + d.id + '/favourite'); load(); } }, d.favourite ? '★' : '☆')));
      }
    }
    app.append(topbar(), h('div', { class: 'page' }, h('h2', {}, 'Dashboards'), list,
      h('h3', { style: 'margin-top:16px' }, 'New dashboard'),
      h('div', { class: 'chips' }, name, cols, h('label', { for: 'dash-shared' }, shared, ' Share with the organization'), h('button', { class: 'btn primary', onclick: async () => {
        msg.textContent = '';
        const r = await api('POST', '/api/v1/dashboards', { name: name.value, columns: Number(cols.value), shared: shared.checked ? 'org' : 'private' });
        if (r.status === 201) location.hash = '#/dashboards/' + r.data.id; else msg.textContent = errText(r);
      } }, 'Create')), msg));
    await load();
  }

  // ---------- gadget bodies (everything from the server is shown as text) ----------
  const link = k => h('a', { href: '#/issue/' + k }, k);
  function issueTable(rows, total) {
    if (!rows || !rows.length) return h('p', { class: 'muted' }, 'No issues.');
    return h('div', {}, h('table', { class: 'dash-table' }, h('thead', {}, h('tr', {}, ...['Key', 'Summary', 'Status', 'Priority', 'Assignee'].map(t => h('th', { scope: 'col' }, t)))),
      h('tbody', {}, ...rows.map(r => h('tr', {}, h('td', {}, link(r.key)), h('td', {}, r.summary), h('td', {}, r.status), h('td', {}, r.priority), h('td', {}, r.assignee || ''))))),
      h('p', { class: 'muted' }, rows.length === total ? total + ' issue(s)' : 'Showing ' + rows.length + ' of ' + total));
  }
  function bars(items, total) {
    return h('ul', { class: 'dash-bars' }, ...items.map(s => h('li', {}, h('span', { class: 'dash-bar-label' }, s.a), h('span', { class: 'dash-bar-track', 'aria-hidden': 'true' }, h('span', { class: 'dash-bar-fill', style: 'width:' + pct(s.count, total) + '%' })), h('span', { class: 'dash-bar-n' }, s.count + ' (' + pct(s.count, total) + '%)'))));
  }
  function flowChart(days) {
    const max = Math.max(1, ...days.flatMap(d => [d.created, d.resolved]));
    const W = 300, H = 90, bw = W / days.length;
    const NS = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(NS, 'svg');
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`); svg.setAttribute('role', 'img'); svg.setAttribute('class', 'dash-flow');
    const tc = days.reduce((a, d) => a + d.created, 0), tr = days.reduce((a, d) => a + d.resolved, 0);
    svg.setAttribute('aria-label', `Created ${tc}, resolved ${tr} in the last ${days.length} days`);
    days.forEach((d, i) => {
      for (const [k, cls, off] of [['created', 'c', 0], ['resolved', 'r', bw / 2]]) {
        const r = document.createElementNS(NS, 'rect'), v = d[k], hh = H * v / max;
        r.setAttribute('x', i * bw + off); r.setAttribute('y', H - hh); r.setAttribute('width', Math.max(1, bw / 2 - 0.5)); r.setAttribute('height', hh); r.setAttribute('class', 'dash-' + cls);
        const t = document.createElementNS(NS, 'title'); t.textContent = d.day + ': ' + k + ' ' + v; r.append(t); svg.append(r);
      }
    });
    return h('div', {}, svg, h('p', { class: 'muted' }, h('span', { class: 'dash-key c' }, '■'), ' created ' + tc + '  ', h('span', { class: 'dash-key r' }, '■'), ' resolved ' + tr));
  }
  function body(g, res) {
    if (res.error) return h('p', { class: 'err', role: 'alert' }, res.error);
    const d = res.data || {};
    switch (g.kind) {
      case 'filter_results': case 'assigned_to_me': return issueTable(d.issues, d.total);
      case 'pie': return d.slices && d.slices.length ? bars(d.slices, d.total) : h('p', { class: 'muted' }, 'Nothing to count.');
      case 'stats_2d': {
        const rs = [...new Set(d.cells.map(c => c.a))], cs = [...new Set(d.cells.map(c => c.b))];
        if (!rs.length) return h('p', { class: 'muted' }, 'Nothing to count.');
        const cell = (a, b) => (d.cells.find(c => c.a === a && c.b === b) || { count: 0 }).count;
        return h('table', { class: 'dash-table' }, h('thead', {}, h('tr', {}, h('th', { scope: 'col' }, d.rows + ' \\ ' + d.cols), ...cs.map(c => h('th', { scope: 'col' }, c)))),
          h('tbody', {}, ...rs.map(a => h('tr', {}, h('th', { scope: 'row' }, a), ...cs.map(c => h('td', {}, String(cell(a, c))))))));
      }
      case 'created_vs_resolved': return flowChart(d.days);
      case 'average_age': return h('div', {}, h('p', { class: 'dash-big' }, d.average_age_days === undefined ? '—' : d.average_age_days + ' days'), h('p', { class: 'muted' }, d.open_issues + ' open issue(s)' + (d.oldest ? '; the oldest is ' : ''), d.oldest ? link(d.oldest) : ''));
      case 'activity_stream': return d.events.length ? h('ul', { class: 'dash-feed' }, ...d.events.map(e => h('li', {}, link(e.key), ' ' + e.field + (e.after ? ' → ' + e.after : '') + ' — ' + e.actor + ', ' + new Date(e.at).toLocaleString()))) : h('p', { class: 'muted' }, 'No activity.');
      case 'workload': return h('table', { class: 'dash-table' }, h('thead', {}, h('tr', {}, ...['Assignee', 'Open', 'Estimate'].map(t => h('th', { scope: 'col' }, t)))), h('tbody', {}, ...(d.rows || []).map(r => h('tr', {}, h('td', {}, r.assignee || 'Unassigned'), h('td', {}, String(r.open)), h('td', {}, String(r.estimate))))));
      case 'sprint_burndown': return h('div', {}, h('p', { class: 'dash-big' }, (d.completed || 0) + ' of ' + (d.committed || 0)), h('p', { class: 'muted' }, 'completed of committed in ' + ((d.sprint && d.sprint.name) || 'the sprint')));
      case 'text': return h('div', {}, ...String(d.markdown || '').split(/\n\n+/).map(p => h('p', {}, p)));
      case 'iframe': return h('iframe', { src: d.url, title: g.title || 'Embedded page', style: 'width:100%;height:' + (d.height || 300) + 'px;border:0', sandbox: 'allow-scripts allow-same-origin allow-forms allow-popups', referrerpolicy: 'no-referrer', loading: 'lazy' });
    }
    return h('p', { class: 'muted' }, 'This gadget cannot be shown.');
  }

  // ---------- the page ----------
  async function dashView(id) {
    const app = clear(document.getElementById('app'));
    const r = await api('GET', '/api/v1/dashboards/' + id);
    if (r.status !== 200) { app.append(topbar(), h('div', { class: 'page' }, h('p', { class: 'err' }, r.status === 404 ? 'That dashboard does not exist, or it is not shared with you.' : errText(r)))); return; }
    const d = r.data; d.gadgets = d.gadgets || [];
    let editing = false;
    const grid = h('div', { class: 'dash-grid dash-cols-' + d.columns });
    const head = h('div', {});
    const msg = h('div', { class: 'err', role: 'alert' });
    const bodies = {};
    async function refreshOne(g) {
      const box = bodies[g.id];
      if (!box) return;
      const x = await api('GET', `/api/v1/dashboards/${id}/gadgets/${g.id}/data`);
      clear(box);
      box.append(x.status === 200 ? body(g, x.data) : h('p', { class: 'err', role: 'alert' }, errText(x)));
    }
    const refreshAll = () => d.gadgets.forEach(refreshOne);
    async function mutate(method, path, payload) {
      msg.textContent = '';
      const x = await api(method, path, payload);
      if (x.status >= 300) { msg.textContent = errText(x); return false; }
      return true;
    }
    async function reload() { await dashView(id); }
    function card(g) {
      const box = h('div', { class: 'dash-body' }, h('p', { class: 'muted' }, 'Loading…'));
      bodies[g.id] = box;
      const tools = editing ? h('div', { class: 'dash-tools' },
        h('button', { class: 'btn ghost', 'aria-label': 'Move ' + (g.title || KIND[g.kind]) + ' left', disabled: g.col === 0, onclick: async () => { if (await mutate('PUT', `/api/v1/dashboards/${id}/gadgets/${g.id}`, { title: g.title, config: g.config, col: g.col - 1 })) reload(); } }, '←'),
        h('button', { class: 'btn ghost', 'aria-label': 'Move ' + (g.title || KIND[g.kind]) + ' right', disabled: g.col >= d.columns - 1, onclick: async () => { if (await mutate('PUT', `/api/v1/dashboards/${id}/gadgets/${g.id}`, { title: g.title, config: g.config, col: g.col + 1 })) reload(); } }, '→'),
        h('button', { class: 'btn ghost', 'aria-label': 'Move ' + (g.title || KIND[g.kind]) + ' up', onclick: async () => { if (await mutate('PUT', `/api/v1/dashboards/${id}/gadgets/${g.id}`, { title: g.title, config: g.config, col: g.col, position: Math.max(0, g.position - 1) })) reload(); } }, '↑'),
        h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + (g.title || KIND[g.kind]), onclick: async () => { if (await mutate('DELETE', `/api/v1/dashboards/${id}/gadgets/${g.id}`)) reload(); } }, 'Remove')) : null;
      return h('section', { class: 'card dash-card', 'aria-label': g.title || KIND[g.kind] }, h('div', { class: 'dash-head' }, h('h3', {}, g.title || KIND[g.kind]), h('button', { class: 'btn ghost', 'aria-label': 'Refresh ' + (g.title || KIND[g.kind]), onclick: () => refreshOne(g) }, '⟳')), tools, box);
    }
    function draw() {
      clear(grid);
      for (let c = 0; c < d.columns; c++) {
        const col = h('div', { class: 'dash-col' });
        for (const g of d.gadgets.filter(x => x.col === c)) col.append(card(g));
        grid.append(col);
      }
      refreshAll();
      clear(head);
      const refresh = h('select', { 'aria-label': 'Auto-refresh' }, ...REFRESH.map(([v, t]) => h('option', { value: v }, t)));
      refresh.value = d.refresh_seconds;
      refresh.disabled = !d.can_edit;
      refresh.addEventListener('change', async () => { if (await mutate('PUT', '/api/v1/dashboards/' + id, { name: d.name, columns: d.columns, shared: d.shared, refresh_seconds: Number(refresh.value) })) reload(); });
      head.append(h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, d.name), h('span', { class: 'muted' }, (d.mine ? 'yours' : 'by ' + d.owner) + (d.shared === 'org' ? ' · shared with the organization' : '')), h('span', { class: 'grow' }),
        h('label', {}, 'Refresh ', refresh),
        h('button', { class: 'btn ghost', 'aria-pressed': d.favourite ? 'true' : 'false', onclick: async () => { if (await mutate(d.favourite ? 'DELETE' : 'PUT', `/api/v1/dashboards/${id}/favourite`)) reload(); } }, d.favourite ? '★ Favourite' : '☆ Favourite'),
        h('button', { class: 'btn ghost', onclick: async () => { if (await mutate('PUT', '/api/v1/me/default-dashboard', { id: d.default ? '' : id })) { toast(d.default ? 'No default dashboard' : 'This dashboard opens by default'); reload(); } } }, d.default ? 'Not my default' : 'Make my default'),
        h('button', { class: 'btn ghost', onclick: async () => { const x = await api('POST', `/api/v1/dashboards/${id}/clone`); if (x.status === 201) location.hash = '#/dashboards/' + x.data.id; else msg.textContent = errText(x); } }, 'Copy'),
        d.can_edit ? h('button', { class: 'btn', 'aria-pressed': editing ? 'true' : 'false', onclick: () => { editing = !editing; draw(); if (editing) editor.hidden = false; else editor.hidden = true; } }, editing ? 'Done editing' : 'Edit') : null));
    }
    // adding a gadget
    const editor = h('div', { class: 'card', style: 'padding:12px;margin:12px 0', hidden: true });
    const kindSel = h('select', { 'aria-label': 'Gadget type' }, ...Object.entries(KIND).map(([k, t]) => h('option', { value: k }, t)));
    const fields = h('div', { class: 'chips' });
    const filters = (await api('GET', '/api/v1/filters')).data || [];
    const kinds = (await api('GET', '/api/v1/dashboard-kinds')).data || { dimensions: [], iframe_hosts: [] };
    const dimOpts = () => kinds.dimensions.sort().map(x => h('option', { value: x }, x));
    const inp = {};
    function drawFields() {
      clear(fields); for (const k in inp) delete inp[k];
      const k = kindSel.value;
      inp.title = h('input', { type: 'text', 'aria-label': 'Gadget title', placeholder: 'Title (optional)', maxlength: 100 });
      fields.append(inp.title);
      if (['filter_results', 'activity_stream', 'pie', 'stats_2d', 'created_vs_resolved', 'average_age'].includes(k)) {
        inp.filter = h('select', { 'aria-label': 'Saved filter' }, h('option', { value: '' }, 'A query…'), ...filters.map(f => h('option', { value: f.id }, f.name + (f.mine ? '' : ' (' + f.owner + ')'))));
        inp.q = h('input', { type: 'text', 'aria-label': 'Query', placeholder: 'project = ABC AND statusCategory != Done', style: 'min-width:260px' });
        fields.append(inp.filter, inp.q);
      }
      if (['filter_results', 'assigned_to_me', 'activity_stream'].includes(k)) { inp.limit = h('input', { type: 'number', 'aria-label': 'How many', min: 1, max: 50, value: 10, style: 'width:80px' }); fields.append(inp.limit); }
      if (k === 'pie') { inp.by = h('select', { 'aria-label': 'Count by' }, ...dimOpts()); fields.append(inp.by); }
      if (k === 'stats_2d') { inp.rows = h('select', { 'aria-label': 'Rows' }, ...dimOpts()); inp.cols = h('select', { 'aria-label': 'Columns of the table' }, ...dimOpts()); inp.cols.selectedIndex = 1; fields.append(inp.rows, inp.cols); }
      if (k === 'created_vs_resolved') { inp.days = h('input', { type: 'number', 'aria-label': 'Days', min: 7, max: 90, value: 30, style: 'width:80px' }); fields.append(inp.days); }
      if (k === 'workload') { inp.board = h('input', { type: 'text', 'aria-label': 'Board id', placeholder: 'board id' }); fields.append(inp.board); }
      if (k === 'sprint_burndown') { inp.sprint = h('input', { type: 'text', 'aria-label': 'Sprint id', placeholder: 'sprint id' }); fields.append(inp.sprint); }
      if (k === 'text') { inp.md = h('textarea', { 'aria-label': 'Text', rows: 3, placeholder: 'Notes for the team', style: 'min-width:320px' }); fields.append(inp.md); }
      if (k === 'iframe') { inp.url = h('input', { type: 'text', 'aria-label': 'Page address', placeholder: kinds.iframe_hosts.length ? 'https://' + kinds.iframe_hosts[0] + '/…' : 'no host may be embedded: ask an administrator', style: 'min-width:260px' }); inp.height = h('input', { type: 'number', 'aria-label': 'Height in pixels', min: 100, max: 1200, value: 300, style: 'width:90px' }); fields.append(inp.url, inp.height); }
    }
    kindSel.addEventListener('change', drawFields);
    editor.append(h('h3', { style: 'margin-top:0' }, 'Add a gadget'), h('div', { class: 'chips' }, kindSel), fields, h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: async () => {
      const k = kindSel.value, cfg = {};
      if (inp.filter && inp.filter.value) cfg.filter_id = inp.filter.value;
      else if (inp.q && inp.q.value.trim()) cfg.q = inp.q.value.trim();
      if (inp.limit) cfg.limit = Number(inp.limit.value);
      if (inp.by) cfg.by = inp.by.value;
      if (inp.rows) { cfg.rows = inp.rows.value; cfg.cols = inp.cols.value; }
      if (inp.days) cfg.days = Number(inp.days.value);
      if (inp.board) cfg.board_id = inp.board.value.trim();
      if (inp.sprint) cfg.sprint_id = inp.sprint.value.trim();
      if (inp.md) cfg.markdown = inp.md.value;
      if (inp.url) { cfg.url = inp.url.value.trim(); cfg.height = Number(inp.height.value); }
      if (await mutate('POST', `/api/v1/dashboards/${id}/gadgets`, { kind: k, title: inp.title.value, col: 0, config: cfg })) { editing = true; sessionStorage.setItem('dash-edit-' + id, '1'); reload(); }
    } }, 'Add gadget')));
    drawFields();
    if (sessionStorage.getItem('dash-edit-' + id) === '1' && d.can_edit) { editing = true; editor.hidden = false; }
    app.append(topbar(), h('div', { class: 'page', style: 'max-width:1400px' }, head, msg, editor, grid));
    draw();
    if (d.refresh_seconds) A.timer(() => { if (document.visibilityState === 'visible') refreshAll(); }, d.refresh_seconds * 1000);
    const stop = () => { try { sessionStorage.removeItem('dash-edit-' + id); } catch (e) { /* none */ } };
    window.addEventListener('hashchange', stop, { once: true });
  }

  A.EXT.routes.push({ re: /^#\/dashboards\/([0-9a-f-]{36})\/?$/, run: m => dashView(m[1]) });
  A.EXT.routes.push({ re: /^#\/dashboards\/?$/, run: () => listView() });
})();
