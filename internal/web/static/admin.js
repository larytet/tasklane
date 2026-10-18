// Administration: the workflow editor (list editor, diagram, draft, check and publish with in-flight migration).
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, S } = A;
  const NS = 'http://www.w3.org/2000/svg';
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const CATS = [['todo', 'To Do'], ['in_progress', 'In progress'], ['done', 'Done']];

  function adminTabs(active) {
    return h('nav', { class: 'tabs', 'aria-label': 'Administration' }, h('span', { class: 'tabs-title' }, 'Administration'),
      h('a', { href: '#/admin/workflows', class: 'tab', 'aria-current': active === 'workflows' ? 'page' : null }, 'Workflows'),
      h('a', { href: '#/admin/audit', class: 'tab', 'aria-current': active === 'audit' ? 'page' : null }, 'Audit log'));
  }

  // ---------- list ----------
  async function listView() {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can edit workflows.'))); return; }
    const list = h('div', { class: 'card', role: 'list' });
    const tpl = h('select', { 'aria-label': 'Template' });
    const name = h('input', { type: 'text', 'aria-label': 'New workflow name', placeholder: 'New workflow name' });
    const msg = h('div', { class: 'err', role: 'alert' });
    app.append(topbar(), adminTabs('workflows'), h('div', { class: 'page' }, h('h2', {}, 'Workflows'), list,
      h('div', { class: 'chips', style: 'margin-top:12px' }, name, tpl, h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/workflows', { name: name.value, template: tpl.value });
        if (r.status === 201) location.hash = '#/admin/workflows/' + r.data.id; else msg.textContent = errText(r);
      } }, 'Create from template')), msg));
    const [r, t] = await Promise.all([api('GET', '/api/v1/workflows'), api('GET', '/api/v1/workflow-templates')]);
    for (const n of t.data || []) tpl.append(h('option', { value: n }, n));
    if (r.status !== 200) { list.append(h('div', { class: 'row' }, 'Workflows could not be loaded.')); return; }
    for (const w of r.data) {
      list.append(h('div', { class: 'row', role: 'listitem', tabindex: 0, onclick: () => { location.hash = '#/admin/workflows/' + w.id; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/admin/workflows/' + w.id; } },
        h('span', { class: 'sum' }, w.name), h('span', { class: 'muted' }, 'v' + w.version + (w.projects.length ? ' · ' + w.projects.join(', ') : ' · not used')),
        w.has_draft ? h('span', { class: 'loz in_progress' }, 'Draft') : null, w.migrating ? h('span', { class: 'loz in_progress' }, 'Migrating') : null));
    }
  }


  // ---------- audit log ----------
  async function auditView() {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can read the audit log.'))); return; }
    const actor = h('input', { type: 'text', 'aria-label': 'Filter by person', placeholder: 'Person' }), action = h('input', { type: 'text', 'aria-label': 'Filter by action', placeholder: 'Action, such as workflow.' });
    const list = h('div', { class: 'card', role: 'list' }), state = h('div', { role: 'status', 'aria-live': 'polite', class: 'muted' }), more = h('button', { class: 'btn', hidden: true }, 'Load more');
    let before = 0;
    const qs = () => 'actor=' + encodeURIComponent(actor.value) + '&action=' + encodeURIComponent(action.value);
    async function load(reset) {
      if (reset) { clear(list); before = 0; }
      const r = await api('GET', '/api/v1/admin/audit?limit=50&' + qs() + (before ? '&before=' + before : ''));
      if (r.status !== 200) { state.textContent = errText(r); return; }
      for (const e of r.data) {
        list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted', style: 'min-width:150px' }, new Date(e.at).toLocaleString()), h('strong', { style: 'min-width:120px' }, e.actor || '—'),
          h('span', { class: 'sum' }, e.action + (e.target ? '  ' + e.target : '')), h('span', { class: 'muted' }, e.detail && e.detail.status ? String(e.detail.status) : '')));
        before = e.id;
      }
      more.hidden = r.data.length < 50;
      if (reset && !r.data.length) list.append(h('div', { class: 'row' }, h('span', { class: 'muted' }, 'No entries match.')));
    }
    more.addEventListener('click', () => load(false));
    const verify = h('button', { class: 'btn', onclick: async () => {
      state.textContent = 'Checking the chain…';
      const r = await api('GET', '/api/v1/admin/audit/verify');
      if (r.status !== 200) { state.textContent = errText(r); return; }
      const c = r.data;
      state.textContent = c.ok ? 'The chain is intact: ' + c.entries + ' entries' + (c.unchained ? ' (' + c.unchained + ' older entries predate it)' : '') + '.' : 'The chain is broken at entry ' + c.broken_at + ': ' + c.problem + '.';
    } }, 'Verify the chain');
    for (const i of [actor, action]) i.addEventListener('change', () => load(true));
    app.append(topbar(), adminTabs('audit'), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Audit log'), h('span', { class: 'grow' }), verify,
      h('a', { class: 'btn', href: '/api/v1/admin/audit/export?' + qs(), download: 'audit.csv', onclick: e => { e.currentTarget.href = '/api/v1/admin/audit/export?' + qs(); } }, 'Export CSV')),
      h('div', { class: 'chips', style: 'margin:8px 0' }, actor, action), state, list, h('div', { style: 'margin-top:8px' }, more)));
    await load(true);
  }

  // ---------- the diagram ----------
  function diagram(def) {
    const W = 170, H = 46, GX = 90, GY = 38;
    const pos = {}, cols = { todo: 0, in_progress: 0, done: 0 };
    def.statuses.forEach(st => {
      const c = Math.max(0, CATS.findIndex(x => x[0] === st.category));
      const row = cols[st.category] = (cols[st.category] || 0) + 1;
      pos[st.name.toLowerCase()] = { x: 20 + c * (W + GX), y: 30 + (row - 1) * (H + GY), st };
    });
    const rows = Math.max(1, ...Object.values(cols));
    const svg = document.createElementNS(NS, 'svg');
    const width = 40 + 3 * W + 2 * GX, height = 60 + rows * (H + GY);
    svg.setAttribute('viewBox', `0 0 ${width} ${height}`); svg.setAttribute('width', '100%'); svg.setAttribute('role', 'img'); svg.setAttribute('class', 'wf-diagram');
    svg.setAttribute('aria-label', 'Workflow diagram: ' + def.statuses.length + ' statuses and ' + def.transitions.length + ' transitions. The lists below hold the same information.');
    const el = (tag, attrs, text) => { const e = document.createElementNS(NS, tag); for (const k in attrs) e.setAttribute(k, attrs[k]); if (text != null) e.textContent = text; return e; };
    const defs = el('defs', {}); const mk = el('marker', { id: 'wf-arrow', viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse' });
    mk.append(el('path', { d: 'M0 0L10 5L0 10z', class: 'wf-arrowhead' })); defs.append(mk); svg.append(defs);
    def.transitions.forEach((t, i) => {
      const to = pos[(t.to || '').toLowerCase()]; if (!to) return;
      const from = t.from ? pos[t.from.toLowerCase()] : null;
      let d, lx, ly;
      if (!from) { // from any status: an arrow from a small marker above the target
        d = `M${to.x + W / 2} ${to.y - 22} L${to.x + W / 2} ${to.y}`; lx = to.x + W / 2 + 6; ly = to.y - 10;
        svg.append(el('text', { x: to.x + W / 2 - 22, y: to.y - 26, class: 'wf-label' }, '★ any'));
      } else {
        const fx = from.x + W / 2, fy = from.y + H / 2, tx = to.x + W / 2, ty = to.y + H / 2;
        const same = from === to;
        const sx = fx + (tx > fx ? W / 2 : tx < fx ? -W / 2 : 0), ex = tx + (tx > fx ? -W / 2 : tx < fx ? W / 2 : 0);
        if (same) { d = `M${from.x + W} ${fy - 8} c40 -30 40 30 0 16`; lx = from.x + W + 30; ly = fy; }
        else if (tx === fx) { const off = (i % 3 - 1) * 24, dir = ty > fy ? 1 : -1; d = `M${fx + off} ${fy + dir * H / 2} L${tx + off} ${ty - dir * H / 2}`; lx = fx + off + 4; ly = (fy + ty) / 2; }
        else { const my = (fy + ty) / 2 + ((i % 3) - 1) * 16; d = `M${sx} ${fy} Q${(sx + ex) / 2} ${my + (fy === ty ? -40 : 0)} ${ex} ${ty}`; lx = (sx + ex) / 2; ly = my + (fy === ty ? -22 : -2); }
      }
      svg.append(el('path', { d, class: 'wf-edge', 'marker-end': 'url(#wf-arrow)' }));
      svg.append(el('text', { x: lx, y: ly, class: 'wf-label', 'text-anchor': 'middle' }, t.name));
    });
    for (const p of Object.values(pos)) {
      const g = el('g', {});
      g.append(el('rect', { x: p.x, y: p.y, width: W, height: H, rx: 8, class: 'wf-node wf-' + p.st.category }));
      g.append(el('text', { x: p.x + W / 2, y: p.y + H / 2 + 5, 'text-anchor': 'middle', class: 'wf-name' }, p.st.name));
      svg.append(g);
    }
    CATS.forEach((c, i) => svg.append(el('text', { x: 20 + i * (W + GX), y: 16, class: 'wf-label wf-cat' }, c[1])));
    return svg;
  }

  // ---------- the editor ----------
  async function editorView(id) {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can edit workflows.'))); return; }
    const body = h('div', { class: 'page' });
    app.append(topbar(), adminTabs('workflows'), body);
    let view, def, dirty = false, poll, tBox;
    const load = async () => {
      const r = await api('GET', '/api/v1/workflows/' + id);
      if (r.status !== 200) { clear(body).append(h('p', {}, 'This workflow could not be loaded.')); return false; }
      view = r.data; def = JSON.parse(JSON.stringify(view.draft || view.live)); dirty = false; return true;
    };
    function render() {
      const editing = !!view.draft;
      clear(body);
      body.append(h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, view.name), h('span', { class: 'muted' }, 'version ' + view.version + (view.projects.length ? ' · used by ' + view.projects.join(', ') : ' · not used by a project')), h('span', { class: 'grow' }),
        editing ? [h('span', { id: 'wf-state', class: 'muted', role: 'status' }, dirty ? 'Unsaved changes' : 'Draft saved'),
          h('button', { class: 'btn', onclick: save }, 'Save draft'), h('button', { class: 'btn primary', onclick: publishDialog }, 'Check and publish'), h('button', { class: 'btn', onclick: discard }, 'Discard draft')]
          : [h('button', { class: 'btn primary', onclick: startDraft }, 'Edit workflow')],
        h('a', { class: 'btn', href: '/api/v1/workflows/' + id + '/export', download: 'workflow.json' }, 'Export JSON')));
      for (const m of view.migrations.filter(m => m.state === 'pending')) {
        body.append(h('div', { class: 'banner info', role: 'status' }, 'Moving issues out of ' + m.status + ' to ' + m.to + ': ' + m.done + ' of ' + m.total,
          h('progress', { max: m.total, value: m.done, 'aria-label': 'Migration progress', style: 'margin-left:8px' })));
      }
      if (!editing) body.append(h('p', { class: 'muted' }, 'This is the live workflow. Start a draft to change it; nothing changes for anyone until you publish.'));
      body.append(h('div', { class: 'card', style: 'padding:8px;margin:8px 0;overflow:auto' }, diagram(def)));
      tBox = transitionsEl(editing);
      body.append(statusesEl(editing), tBox);
    }
    // a renamed status changes the choices in the transition lists: only that card is rebuilt, so a click on a status button is never lost to a re-render
    const refreshTransitions = () => { if (!tBox) return; const n = transitionsEl(!!view.draft); tBox.replaceWith(n); tBox = n; };
    const touch = () => { dirty = true; const s = document.getElementById('wf-state'); if (s) s.textContent = 'Unsaved changes'; const d = body.querySelector('.wf-diagram'); if (d) d.replaceWith(diagram(def)); };
    function statusesEl(editing) {
      const box = h('div', { class: 'card', style: 'padding:12px;margin:8px 0' }, h('strong', {}, 'Statuses'));
      def.statuses.forEach((st, i) => {
        const name = h('input', { type: 'text', value: st.name, 'aria-label': 'Status name', disabled: !editing }), cat = h('select', { 'aria-label': 'Category of ' + st.name, disabled: !editing }, CATS.map(c => h('option', { value: c[0], selected: c[0] === st.category }, c[1])));
        name.addEventListener('input', () => { const old = st.name; st.name = name.value; for (const t of def.transitions) { if (t.from === old) t.from = st.name; if (t.to === old) t.to = st.name; } touch(); });
        name.addEventListener('change', refreshTransitions);
        cat.addEventListener('change', () => { st.category = cat.value; touch(); });
        box.append(h('div', { class: 'chips' }, name, cat, editing ? [
          h('button', { class: 'btn ghost', 'aria-label': 'Move ' + st.name + ' up', disabled: i === 0, onclick: () => { def.statuses.splice(i - 1, 0, def.statuses.splice(i, 1)[0]); touch(); render(); } }, '↑'),
          h('button', { class: 'btn ghost', 'aria-label': 'Move ' + st.name + ' down', disabled: i === def.statuses.length - 1, onclick: () => { def.statuses.splice(i + 1, 0, def.statuses.splice(i, 1)[0]); touch(); render(); } }, '↓'),
          h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + st.name, onclick: () => { def.transitions = def.transitions.filter(t => t.from !== st.name && t.to !== st.name); def.statuses.splice(i, 1); touch(); render(); } }, 'Remove')] : null));
      });
      if (editing) box.append(h('button', { class: 'btn', onclick: () => { def.statuses.push({ name: 'New status ' + (def.statuses.length + 1), category: 'in_progress' }); touch(); render(); } }, 'Add status'));
      return box;
    }
    function transitionsEl(editing) {
      const box = h('div', { class: 'card', style: 'padding:12px;margin:8px 0' }, h('strong', {}, 'Transitions'));
      def.transitions.forEach((t, i) => {
        const name = h('input', { type: 'text', value: t.name, 'aria-label': 'Transition name', disabled: !editing });
        const from = h('select', { 'aria-label': 'From status of ' + t.name, disabled: !editing }, h('option', { value: '' }, 'Any status'), def.statuses.map(s => h('option', { value: s.name, selected: s.name === t.from }, s.name)));
        const to = h('select', { 'aria-label': 'To status of ' + t.name, disabled: !editing }, def.statuses.map(s => h('option', { value: s.name, selected: s.name === t.to }, s.name)));
        const human = h('input', { type: 'checkbox', checked: !!t.requires_human, 'aria-label': 'Only a person can take ' + t.name, disabled: !editing });
        name.addEventListener('input', () => { t.name = name.value; touch(); });
        from.addEventListener('change', () => { t.from = from.value; touch(); });
        to.addEventListener('change', () => { t.to = to.value; touch(); });
        human.addEventListener('change', () => { t.requires_human = human.checked; touch(); });
        const n = ['conditions', 'validators', 'post_functions'].reduce((a, k) => a + ((t.rules && t.rules[k]) || []).length, 0);
        box.append(h('div', { class: 'chips' }, name, from, '→', to, h('label', { class: 'muted' }, human, ' people only'),
          h('button', { class: 'btn ghost', 'aria-label': 'Rules of ' + t.name, onclick: () => rulesDialog(t, editing) }, 'Rules' + (n ? ' (' + n + ')' : '')),
          editing ? h('button', { class: 'btn ghost', 'aria-label': 'Remove transition ' + t.name, onclick: () => { def.transitions.splice(i, 1); touch(); render(); } }, 'Remove') : null));
      });
      if (editing) box.append(h('button', { class: 'btn', onclick: () => { def.transitions.push({ name: 'New transition', from: def.statuses[0] ? def.statuses[0].name : '', to: def.statuses[0] ? def.statuses[0].name : '', rules: {} }); touch(); render(); } }, 'Add transition'));
      return box;
    }
    // Conditions, validators, post-functions and the transition screen are edited as JSON lists; the server checks them when saved.
    function rulesDialog(t, editing) {
      t.rules = t.rules || {};
      const mkTa = (k, label, hint) => { const ta = h('textarea', { 'aria-label': label, rows: 4, disabled: !editing, spellcheck: 'false', style: 'width:100%;font-family:monospace' }); ta.value = JSON.stringify(t.rules[k] || [], null, 1); return [h('label', {}, h('strong', {}, label), ' ', h('span', { class: 'muted' }, hint)), ta]; };
      const c = mkTa('conditions', 'Conditions', 'role, assignee_only, reporter_only, assignee_or_reporter, field, subtasks_resolved'), v = mkTa('validators', 'Validators', 'field_required, regex, permission, previous_status'),
        p = mkTa('post_functions', 'Post-functions', 'set_field, assign, add_comment, webhook, create_subtask'), sc = mkTa('screen', 'Transition screen', 'field keys asked for first');
      const msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Rules of ' + t.name), c, v, p, sc, msg, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, editing ? 'Cancel' : 'Close'),
        editing ? h('button', { class: 'btn primary', onclick: () => {
          try { const next = {}; [['conditions', c[1]], ['validators', v[1]], ['post_functions', p[1]], ['screen', sc[1]]].forEach(([k, ta]) => { const x = JSON.parse(ta.value || '[]'); if (!Array.isArray(x)) throw new Error(k + ' must be a list'); if (x.length) next[k] = x; }); t.rules = next; touch(); close(); render(); }
          catch (e) { msg.textContent = 'Not valid JSON: ' + e.message; }
        } }, 'Apply') : null)));
    }
    async function startDraft() { const r = await api('POST', '/api/v1/workflows/' + id + '/draft'); if (r.status === 200) { view = r.data; def = JSON.parse(JSON.stringify(view.draft)); dirty = false; render(); } else toast(errText(r)); }
    async function save() { const r = await api('PUT', '/api/v1/workflows/' + id + '/draft', def); if (r.status === 200) { view = r.data; dirty = false; render(); toast('Draft saved'); } else toast(errText(r)); }
    function discard() {
      const close = modal(h('div', {}, h('h2', {}, 'Discard the draft?'), h('p', {}, 'Your unpublished changes are lost. The live workflow is not affected.'),
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Keep editing'), h('button', { class: 'btn primary', onclick: async () => {
          const r = await api('DELETE', '/api/v1/workflows/' + id + '/draft'); close();
          if (r.status === 200 && await load()) render(); else toast(errText(r));
        } }, 'Discard draft'))));
    }
    async function publishDialog() {
      const sv = await api('PUT', '/api/v1/workflows/' + id + '/draft', def); // check what is on screen
      if (sv.status !== 200) { toast(errText(sv)); return; }
      view = sv.data; dirty = false;
      const out = h('div', { 'aria-live': 'polite' }), msg = h('div', { class: 'err', role: 'alert' }), note = h('input', { type: 'text', 'aria-label': 'Version note', placeholder: 'What changed (optional)', style: 'width:100%' });
      const go = h('button', { class: 'btn primary', disabled: true }, 'Publish'); const maps = {};
      async function check() {
        const r = await api('POST', '/api/v1/workflows/' + id + '/draft/check', { mapping: maps });
        clear(out); go.disabled = true;
        if (r.status !== 200) { out.append(h('div', { class: 'err' }, errText(r))); return; }
        const p = r.data, list = (label, items, cls) => items && items.length ? h('div', { class: cls || '' }, h('strong', {}, label), h('ul', {}, items.map(x => h('li', {}, x)))) : null;
        out.append(list('Cannot publish yet', p.errors, 'err'), list('Warnings', p.warnings), list('New statuses', p.added), list('Renamed', p.renamed));
        for (const rm of p.removed) {
          if (!rm.issues) { out.append(h('div', {}, rm.name + ' is removed (no issues are in it).')); continue; }
          const sel = h('select', { 'aria-label': 'Move the ' + rm.issues + ' issues in ' + rm.name + ' to' }, h('option', { value: '' }, 'Choose a status…'), def.statuses.map(s => h('option', { value: s.name, selected: maps[rm.name] === s.name }, s.name)));
          sel.addEventListener('change', () => { maps[rm.name] = sel.value; check(); });
          out.append(h('div', { class: 'chips' }, h('span', {}, rm.issues + ' issue(s) are in ' + rm.name + '. Move them to'), sel));
        }
        go.disabled = p.errors.length > 0;
      }
      go.addEventListener('click', async () => {
        const r = await api('POST', '/api/v1/workflows/' + id + '/publish', { mapping: maps, note: note.value });
        if (r.status === 200) { close(); toast('Published version ' + r.data.version + (r.data.migrations.length ? '. Issues are being moved in the background.' : '.')); if (await load()) render(); }
        else msg.textContent = errText(r);
      });
      const close = modal(h('div', {}, h('h2', {}, 'Publish ' + view.name), out, note, msg, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), go)));
      check();
    }
    if (!await load()) return;
    render();
    // migration progress
    poll = A.timer(async () => {
      if (!view || !view.migrations.some(m => m.state === 'pending') || document.getElementById('wf-state') && dirty) return;
      const r = await api('GET', '/api/v1/workflows/' + id); if (r.status !== 200) return;
      const had = view.migrations.some(m => m.state === 'pending'); view.migrations = r.data.migrations; view.version = r.data.version;
      if (had && !dirty && !document.querySelector('.scrim')) render();
    }, 2000);
  }

  A.EXT.routes.push({ re: /^#\/admin\/workflows\/([0-9a-f-]{36})/, run: m => editorView(m[1]) });
  A.EXT.routes.push({ re: /^#\/admin\/workflows\/?$/, run: () => listView() });
  A.EXT.routes.push({ re: /^#\/admin\/audit\/?$/, run: () => auditView() });
})();
