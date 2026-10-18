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
      h('a', { href: '#/admin/automation', class: 'tab', 'aria-current': active === 'automation' ? 'page' : null }, 'Automation'),
      h('a', { href: '#/admin/identity', class: 'tab', 'aria-current': active === 'identity' ? 'page' : null }, 'Identity'),
      h('a', { href: '#/admin/email', class: 'tab', 'aria-current': active === 'email' ? 'page' : null }, 'Email'),
      h('a', { href: '#/admin/git', class: 'tab', 'aria-current': active === 'git' ? 'page' : null }, 'Git'),
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
      if (reset && !r.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No entries match.')));
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


  // ---------- automation ----------
  const RULE_TEMPLATE = { name: 'New rule', enabled: false, trigger: { type: 'issue.transitioned', to: 'Done' }, conditions: [], actions: [{ kind: 'comment', text: '{{issue.key}} is done' }] };
  async function automationView(project) {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can edit automation rules.'))); return; }
    project = project || (S.projects[0] && S.projects[0].key) || '';
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === project }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/admin/automation/' + sel.value; });
    const list = h('div', { class: 'card', role: 'list' });
    app.append(topbar(), adminTabs('automation'), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Automation'), sel, h('span', { class: 'grow' }),
      h('button', { class: 'btn primary', onclick: () => editRule(null) }, 'New rule')), h('p', { class: 'muted' }, 'A rule runs in the background when its trigger happens, as "Automation: <name>", through the same workflow checks a person meets. Use a dry run to see what a rule would do to an issue.'), list));
    async function load() {
      clear(list);
      const r = await api('GET', '/api/v1/automation/rules?project=' + project);
      if (r.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'This project has no automation rules.')));
      for (const ru of r.data) {
        const on = h('input', { type: 'checkbox', checked: ru.enabled, 'aria-label': 'Enable ' + ru.name });
        on.addEventListener('change', async () => { const x = await api('PUT', '/api/v1/automation/rules/' + ru.id, Object.assign({}, ru, { enabled: on.checked })); if (x.status !== 200) { toast(errText(x)); on.checked = !on.checked; } });
        list.append(h('div', { class: 'row', role: 'listitem' }, on, h('span', { class: 'sum' }, h('strong', {}, ru.name), ' ', h('span', { class: 'muted' }, ru.trigger.type + (ru.trigger.to ? ' → ' + ru.trigger.to : '') + ' · ' + ru.actions.length + ' action(s)')),
          h('button', { class: 'btn ghost', 'aria-label': 'Edit ' + ru.name, onclick: () => editRule(ru) }, 'Edit'),
          h('button', { class: 'btn ghost', 'aria-label': 'Dry run ' + ru.name, onclick: () => dryRun(ru) }, 'Dry run'),
          h('button', { class: 'btn ghost', 'aria-label': 'Runs of ' + ru.name, onclick: () => runsDialog(ru) }, 'Runs'),
          h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + ru.name, onclick: () => deleteRule(ru) }, 'Delete')));
      }
    }
    function editRule(ru) {
      const cur = ru || RULE_TEMPLATE;
      const name = h('input', { type: 'text', value: cur.name, 'aria-label': 'Rule name', style: 'width:100%' });
      const ta = (label, v) => { const t = h('textarea', { 'aria-label': label, rows: 5, spellcheck: 'false', style: 'width:100%;font-family:monospace' }); t.value = JSON.stringify(v, null, 1); return t; };
      const trig = ta('Trigger', cur.trigger), cond = ta('Conditions', cur.conditions || []), acts = ta('Actions', cur.actions);
      const enabled = h('input', { type: 'checkbox', checked: cur.enabled });
      const msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, ru ? 'Edit ' + ru.name : 'New rule'), name,
        h('label', { style: 'display:flex;gap:6px;align-items:center' }, enabled, 'Enabled'),
        h('div', {}, h('strong', {}, 'Trigger '), h('span', { class: 'muted' }, 'issue.created, issue.updated (fields), issue.transitioned (from, to), comment.added (contains), schedule (every_minutes or daily_at, query)'), trig),
        h('div', {}, h('strong', {}, 'Conditions '), h('span', { class: 'muted' }, 'query (TQL), field (field, op, value)'), cond),
        h('div', {}, h('strong', {}, 'Actions '), h('span', { class: 'muted' }, 'transition, assign, set_field, comment, create_subtask, webhook, notify; smart values such as {{issue.key}}'), acts), msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          let body;
          try { body = { name: name.value, project, enabled: enabled.checked, trigger: JSON.parse(trig.value), conditions: JSON.parse(cond.value || '[]'), actions: JSON.parse(acts.value) }; } catch (e) { msg.textContent = 'Not valid JSON: ' + e.message; return; }
          const r = ru ? await api('PUT', '/api/v1/automation/rules/' + ru.id, body) : await api('POST', '/api/v1/automation/rules', body);
          if (r.status === 200 || r.status === 201) { close(); toast('Rule saved'); load(); } else msg.textContent = errText(r);
        } }, 'Save'))));
    }
    function dryRun(ru) {
      const key = h('input', { type: 'text', 'aria-label': 'Issue key', placeholder: project + '-1' }), out = h('div', { 'aria-live': 'polite' });
      const go = async () => {
        clear(out);
        const r = await api('POST', '/api/v1/automation/rules/' + ru.id + '/dry-run', { issue: key.value });
        if (r.status !== 200) { out.append(h('div', { class: 'err' }, errText(r))); return; }
        const d = r.data;
        out.append(h('strong', {}, d.would_run ? 'The rule would run on ' + d.issue + '.' : 'The rule would not run: a condition fails.'),
          h('ul', {}, d.conditions.map(c => h('li', {}, 'Condition ' + c.kind + ': ' + (c.passed ? 'passes' : 'fails — ' + c.why)))),
          h('ul', {}, d.actions.map(a => h('li', { class: a.problem ? 'err' : '' }, a.problem ? a.kind + ': ' + a.problem : a.would))));
      };
      const close = modal(h('div', {}, h('h2', {}, 'Dry run: ' + ru.name), h('div', { class: 'chips' }, key, h('button', { class: 'btn', onclick: go }, 'Show what it would do')), out,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Close'))));
    }
    async function runsDialog(ru) {
      const body = h('div', { role: 'list' });
      const close = modal(h('div', {}, h('h2', {}, 'Runs of ' + ru.name), body, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Close'))));
      const r = await api('GET', '/api/v1/automation/rules/' + ru.id + '/runs');
      if (r.status !== 200) { body.append(h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.length) body.append(h('div', { role: 'listitem', class: 'muted' }, 'No runs yet.'));
      for (const x of r.data) body.append(h('div', { role: 'listitem' }, new Date(x.started).toLocaleString() + ' · ' + x.issue + ' · ' + x.state + (x.detail && (x.detail.error || x.detail.reason) ? ' — ' + (x.detail.error || x.detail.reason) : '')));
    }
    function deleteRule(ru) {
      const close = modal(h('div', {}, h('h2', {}, 'Delete ' + ru.name + '?'), h('p', {}, 'The rule and its run history are removed.'), h('div', { class: 'actions' },
        h('button', { class: 'btn', onclick: () => close() }, 'Keep it'), h('button', { class: 'btn primary', onclick: async () => { const r = await api('DELETE', '/api/v1/automation/rules/' + ru.id); close(); if (r.status === 200) load(); else toast(errText(r)); } }, 'Delete'))));
    }
    await load();
  }


  // ---------- identity: single sign-on providers and SCIM tokens ----------
  async function identityView() {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can configure sign-in.'))); return; }
    const provList = h('div', { role: 'list' }), tokList = h('div', { role: 'list' });
    const section = (title, ...kids) => h('section', { class: 'card', style: 'padding:12px;margin:12px 0', 'aria-label': title }, h('h2', { style: 'margin:0 0 8px' }, title), ...kids);
    app.append(topbar(), adminTabs('identity'), h('div', { class: 'page' },
      section('Single sign-on (OpenID Connect)', h('p', { class: 'muted' }, 'People sign in at your identity provider; Tasklane never sees their password. The redirect address to register there is ' + location.origin + '/api/v1/auth/sso/callback'), provList, h('button', { class: 'btn primary', onclick: () => editProvider(null) }, 'Add a provider')),
      section('SCIM provisioning', h('p', { class: 'muted' }, 'Point your identity provider at ' + location.origin + '/scim/v2 with one of these tokens. Deactivating a person there ends their sessions and tokens here at once.'), tokList,
        h('button', { class: 'btn', onclick: newToken }, 'Create a SCIM token'))));
    async function loadProviders() {
      clear(provList);
      const r = await api('GET', '/api/v1/admin/sso/providers');
      if (r.status !== 200) { provList.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) provList.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No providers yet.')));
      for (const p of r.data) provList.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, p.label), ' ', h('span', { class: 'muted' }, p.issuer + (p.enabled ? '' : ' (off)'))),
        h('button', { class: 'btn ghost', 'aria-label': 'Edit ' + p.label, onclick: () => editProvider(p) }, 'Edit'),
        h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + p.label, onclick: () => { const close = modal(h('div', {}, h('h2', {}, 'Delete ' + p.label + '?'), h('p', {}, 'People who sign in only through it will not be able to until another is set up.'),
          h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Keep it'), h('button', { class: 'btn primary', onclick: async () => { const x = await api('DELETE', '/api/v1/admin/sso/providers/' + p.id); close(); if (x.status === 200) loadProviders(); else toast(errText(x)); } }, 'Delete')))); } }, 'Delete')));
    }
    function editProvider(p) {
      const f = (label, value, type) => { const i = h('input', { type: type || 'text', 'aria-label': label, value: value || '', style: 'width:100%' }); return i; };
      const name = f('Name', p && p.name), label = f('Button label', p && p.label), issuer = f('Issuer address', p && p.issuer), client = f('Client id', p && p.client_id), secret = f('Client secret', '', 'password');
      secret.placeholder = p && p.secret_set ? 'unchanged' : '';
      const domains = f('Allowed email domains', p && (p.allowed_domains || []).join(', ')), scopes = f('Scopes', p ? p.scopes : 'openid email profile');
      const jit = h('input', { type: 'checkbox', checked: p ? p.jit : true }), on = h('input', { type: 'checkbox', checked: p ? p.enabled : true }), msg = h('div', { class: 'err', role: 'alert' });
      const row = (l, el) => h('label', { style: 'display:block;margin-top:6px' }, h('span', { class: 'muted' }, l), el);
      const close = modal(h('div', {}, h('h2', {}, p ? 'Edit ' + p.label : 'Add a provider'), row('Name (letters, digits, hyphens)', name), row('Button label', label), row('Issuer address', issuer), row('Client id', client), row('Client secret', secret),
        row('Allowed email domains (empty: any the provider vouches for)', domains), row('Scopes', scopes),
        h('label', { style: 'display:flex;gap:6px;align-items:center;margin-top:6px' }, jit, 'Create accounts at first sign-in'), h('label', { style: 'display:flex;gap:6px;align-items:center' }, on, 'Enabled'), msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const body = { name: name.value, label: label.value, issuer: issuer.value, client_id: client.value, client_secret: secret.value, scopes: scopes.value, allowed_domains: domains.value.split(/[\s,]+/).filter(Boolean), jit: jit.checked, enabled: on.checked };
          const r = p ? await api('PUT', '/api/v1/admin/sso/providers/' + p.id, body) : await api('POST', '/api/v1/admin/sso/providers', body);
          if (r.status === 200 || r.status === 201) { close(); toast('Provider saved'); loadProviders(); } else msg.textContent = errText(r);
        } }, 'Save'))));
    }
    async function loadTokens() {
      clear(tokList);
      const r = await api('GET', '/api/v1/admin/scim/tokens');
      if (r.status !== 200) { tokList.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) tokList.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No SCIM tokens.')));
      for (const t of r.data) tokList.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, t.name + (t.revoked ? ' (revoked)' : '')), h('span', { class: 'muted' }, 'created ' + new Date(t.created).toLocaleString()),
        t.revoked ? null : h('button', { class: 'btn ghost', 'aria-label': 'Revoke SCIM token ' + t.name, onclick: async () => { const x = await api('DELETE', '/api/v1/admin/scim/tokens/' + t.id); if (x.status === 200) loadTokens(); else toast(errText(x)); } }, 'Revoke')));
    }
    function newToken() {
      const name = h('input', { type: 'text', 'aria-label': 'Token name', placeholder: 'Name, such as okta', style: 'width:100%' }), msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Create a SCIM token'), name, msg, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
        const r = await api('POST', '/api/v1/admin/scim/tokens', { name: name.value });
        if (r.status !== 201) { msg.textContent = errText(r); return; }
        close(); loadTokens();
        const c2 = modal(h('div', {}, h('h2', {}, 'Your SCIM token'), h('p', {}, 'Copy it now. It is not shown again.'), h('pre', { 'aria-label': 'SCIM token', style: 'user-select:all;white-space:pre-wrap;word-break:break-all' }, r.data.token),
          h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => c2() }, 'Done'))));
      } }, 'Create'))));
    }
    await Promise.all([loadProviders(), loadTokens()]);
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

  // ---------- inbound email ----------
  async function emailView() {
    const app = clear(document.getElementById('app'));
    if (!S.me.admin) { app.append(topbar(), h('div', { class: 'page' }, h('p', {}, 'Only administrators can manage email.'))); return; }
    const outcome = h('select', { 'aria-label': 'Outcome' }, [['quarantined', 'Quarantined'], ['', 'All'], ['accepted', 'Accepted'], ['ignored', 'Ignored'], ['rejected', 'Rejected']].map(o => h('option', { value: o[0] }, o[1])));
    const queue = h('div', { class: 'card', role: 'list', 'aria-label': 'Inbound messages' });
    const sources = h('div', { class: 'card', role: 'list', 'aria-label': 'Mail sources' });
    const supp = h('div', { class: 'card', role: 'list', 'aria-label': 'Suppressed addresses' });
    const boxes = h('div', { class: 'card', role: 'list', 'aria-label': 'Mailboxes' });
    const bmsg = h('div', { class: 'err', role: 'alert' });
    const bf = {};
    for (const [k, label, type, ph] of [['name', 'Mailbox name', 'text', 'Support'], ['host', 'IMAP host', 'text', 'imap.example.com'], ['port', 'IMAP port', 'number', '993'], ['username', 'IMAP user', 'text', 'user'], ['password', 'IMAP password', 'password', 'password'], ['mailbox', 'Folder', 'text', 'INBOX']]) bf[k] = h('input', { type, 'aria-label': label, placeholder: ph, autocomplete: 'off', style: k === 'port' ? 'width:90px' : '' });
    const btls = h('select', { 'aria-label': 'Connection security' }, h('option', { value: 'tls' }, 'TLS'), h('option', { value: 'none' }, 'None (this machine only)'));
    const btrust = h('input', { type: 'checkbox', 'aria-label': 'Trusted mailbox' });
    const msg = h('div', { class: 'err', role: 'alert' });
    const name = h('input', { type: 'text', 'aria-label': 'New source name', placeholder: 'Source name' });
    const trusted = h('input', { type: 'checkbox', 'aria-label': 'Trusted receiver' });
    async function loadSources() {
      clear(sources);
      const r = await api('GET', '/api/v1/admin/mail/sources');
      if (r.status !== 200) { sources.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) sources.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No sources. Add one to receive mail by webhook.')));
      for (const x of r.data) sources.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, x.name), ' ', h('span', { class: 'muted' }, (x.trusted ? 'trusted receiver' : 'untrusted') + ' · POST /api/v1/mail/inbound/' + x.id)),
        h('button', { class: 'btn ghost', 'aria-label': 'Delete source ' + x.name, onclick: async () => { const d = await api('DELETE', '/api/v1/admin/mail/sources/' + x.id); if (d.status === 200) loadSources(); else toast(errText(d)); } }, 'Delete')));
    }
    async function loadBoxes() {
      clear(boxes);
      const r = await api('GET', '/api/v1/admin/mail/imap');
      if (r.status !== 200) { boxes.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) boxes.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No mailbox is read. Add one to receive replies by IMAP.')));
      for (const x of r.data) boxes.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, x.name), ' ', h('span', { class: 'muted' }, x.username + '@' + x.host + ':' + x.port + ' · ' + x.mailbox + (x.trusted ? ' · trusted' : '') + ' · ' + x.received + ' received' + (x.uidvalidity_resets ? ' · ' + x.uidvalidity_resets + ' reset(s)' : '') + (x.last_ok ? ' · last read ' + new Date(x.last_ok).toLocaleTimeString() : ' · not read yet'))), x.last_error ? h('span', { class: 'err' }, x.last_error) : null,
        h('button', { class: 'btn ghost', 'aria-label': 'Check mailbox ' + x.name, onclick: async () => { const d = await api('POST', '/api/v1/admin/mail/imap/' + x.id + '/check'); toast(d.status === 200 ? 'Connected: ' + d.data.messages + ' message(s)' + (d.data.idle ? ', IDLE available' : ', no IDLE (polled every 30 s)') : errText(d)); } }, 'Check'),
        h('button', { class: 'btn ghost', 'aria-label': 'Delete mailbox ' + x.name, onclick: async () => { const d = await api('DELETE', '/api/v1/admin/mail/imap/' + x.id); if (d.status === 200) loadBoxes(); else toast(errText(d)); } }, 'Delete')));
    }
    async function loadSupp() {
      clear(supp);
      const r = await api('GET', '/api/v1/admin/mail/suppressions');
      if (r.status !== 200) { supp.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      const live = r.data.filter(x => !x.cleared);
      if (!live.length) supp.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No address is suppressed.')));
      for (const x of live) supp.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, x.email), ' ', x.user ? '(' + x.user + ') ' : '', h('span', { class: 'muted' }, x.kind + ' · ' + x.reason + ' · since ' + new Date(x.since).toLocaleDateString())),
        h('button', { class: 'btn ghost', 'aria-label': 'Clear suppression of ' + x.email, onclick: async () => { const d = await api('DELETE', '/api/v1/admin/mail/suppressions/' + encodeURIComponent(x.email)); if (d.status === 200) loadSupp(); else toast(errText(d)); } }, 'Clear')));
    }
    async function loadQueue() {
      clear(queue);
      const r = await api('GET', '/api/v1/admin/mail/inbound?limit=100&outcome=' + encodeURIComponent(outcome.value));
      if (r.status !== 200) { queue.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      if (!r.data.length) queue.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No messages.')));
      for (const m of r.data) {
        const acts = [];
        if (m.raw) acts.push(h('a', { class: 'btn ghost', href: '/api/v1/admin/mail/inbound/' + m.id + '/raw', download: 'message.eml', 'aria-label': 'Original of message ' + m.id }, 'Original'));
        if (m.outcome === 'quarantined') {
          acts.push(h('button', { class: 'btn', 'aria-label': 'Release message ' + m.id, onclick: async () => { const d = await api('POST', '/api/v1/admin/mail/inbound/' + m.id + '/release'); if (d.status === 200) { toast('Released as a comment'); loadQueue(); } else toast(errText(d)); } }, 'Release'));
          acts.push(h('button', { class: 'btn ghost', 'aria-label': 'Delete message ' + m.id, onclick: async () => { const d = await api('DELETE', '/api/v1/admin/mail/inbound/' + m.id); if (d.status === 200) loadQueue(); else toast(errText(d)); } }, 'Delete'));
        }
        queue.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, m.sender || '(no sender)'), ' ', m.subject, ' ',
          h('span', { class: 'muted' }, new Date(m.received).toLocaleString() + ' · ' + m.outcome + (m.reason ? ': ' + m.reason : '') + (m.issue ? ' · ' + m.issue : '') + (m.duplicates ? ' · ' + m.duplicates + ' repeat(s)' : ''))), ...acts));
      }
    }
    outcome.addEventListener('change', loadQueue);
    app.append(topbar(), adminTabs('email'), h('div', { class: 'page' }, h('h2', {}, 'Inbound email'),
      h('p', { class: 'muted' }, 'Replies to notifications become comments only with a signed reply address, a known sender and a proven origin. Everything else waits here for a decision, and nothing is ever sent back to the sender.'),
      h('h3', {}, 'Sources'), sources,
      h('div', { class: 'chips', style: 'margin:8px 0' }, name, h('label', {}, trusted, ' Trusted receiver (its Authentication-Results header is believed)'), h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/admin/mail/sources', { name: name.value, trusted: trusted.checked });
        if (r.status === 201) { name.value = ''; msg.textContent = ''; loadSources(); const close = modal(h('div', {}, h('h2', {}, 'Signing secret'), h('p', {}, 'Sign each request body with this secret (HMAC-SHA256, header X-Tasklane-Signature: sha256=<hex>). It is shown once.'), h('pre', { class: 'code', 'aria-label': 'Signing secret' }, r.data.secret), h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Done')))); }
        else msg.textContent = errText(r);
      } }, 'Add source')), msg,
      h('h3', { style: 'margin-top:16px' }, 'Mailboxes (IMAP)'), h('p', { class: 'muted' }, 'Tasklane reads these folders (waiting with IDLE where the server has it) and treats each message like one delivered by webhook. Mark a mailbox trusted only if the mail reaches it through a server you run, whose Authentication-Results header you believe; otherwise Tasklane checks DKIM itself.'), boxes,
      h('div', { class: 'chips', style: 'margin:8px 0' }, bf.name, bf.host, bf.port, btls, bf.username, bf.password, bf.mailbox, h('label', {}, btrust, ' Trusted'), h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/admin/mail/imap', { name: bf.name.value, host: bf.host.value, port: Number(bf.port.value) || 0, tls: btls.value, username: bf.username.value, password: bf.password.value, mailbox: bf.mailbox.value, trusted: btrust.checked });
        if (r.status === 201) { bmsg.textContent = ''; for (const k in bf) bf[k].value = ''; loadBoxes(); } else bmsg.textContent = errText(r);
      } }, 'Add mailbox')), bmsg,
      h('h3', { style: 'margin-top:16px' }, 'Suppressed addresses'), h('p', { class: 'muted' }, 'Email stops after a hard bounce, a complaint, or three soft bounces in a week. The in-app inbox is never affected.'), supp,
      h('div', { class: 'chips', style: 'margin-top:16px' }, h('h3', { style: 'margin:0' }, 'Messages'), outcome, h('span', { class: 'grow' }),
        h('button', { class: 'btn ghost', onclick: async () => { const r = await api('POST', '/api/v1/admin/mail/reply-keys'); toast(r.status === 201 ? 'New reply-address key ' + r.data.kid + ' is signing. Older addresses keep working until their key is revoked.' : errText(r)); } }, 'Rotate reply key')),
      queue));
    await Promise.all([loadSources(), loadQueue(), loadSupp(), loadBoxes()]);
  }


  // Git: connections and their health, mapped repositories, visibility, identity links, and per-project smart commits and rules.
  async function gitView() {
    const app = document.getElementById('app'); clear(app);
    const msg = h('div', { class: 'err', role: 'alert' }), rmsg = h('div', { class: 'err', role: 'alert' }), imsg = h('div', { class: 'err', role: 'alert' }), umsg = h('div', { class: 'err', role: 'alert' });
    const conns = h('div', { class: 'list', role: 'list', 'aria-label': 'Connections' });
    const repos = h('div', { class: 'list', role: 'list', 'aria-label': 'Repositories' });
    const ids = h('div', { class: 'list', role: 'list', 'aria-label': 'Linked accounts' });
    const vis = h('div', {});
    const rules = h('div', { class: 'list', role: 'list', 'aria-label': 'Rules' });
    const runs = h('div', { class: 'list', role: 'list', 'aria-label': 'Rule runs' });
    const row = (...c) => h('div', { class: 'row', role: 'listitem' }, ...c);
    const none = t => row(h('span', { class: 'muted' }, t));
    const btn = (label, aria, fn, cls) => h('button', { class: 'btn ' + (cls || 'ghost'), 'aria-label': aria, onclick: fn }, label);
    const projSel = h('select', { 'aria-label': 'Project for rules and smart commits' });
    const commits = h('select', { 'aria-label': 'Smart commits' }, h('option', { value: 'off' }, 'Off'), h('option', { value: 'default_branch' }, 'Commits on the default branch'), h('option', { value: 'any' }, 'Commits on any branch'));
    async function loadConns() {
      clear(conns);
      const r = await api('GET', '/api/v1/git/connections');
      if (r.status !== 200) { conns.append(row(errText(r))); return; }
      if (!r.data.length) conns.append(none('No connection. Add one to link pull requests, branches, builds and deployments to issues.'));
      for (const c of r.data) {
        const health = c.disconnected ? 'disconnected (read-only)' : c.health + (c.health_detail ? ': ' + c.health_detail : '');
        conns.append(row(h('span', { class: 'sum' }, h('strong', {}, c.name), ' ', h('span', { class: 'muted' }, c.provider + ' · ' + c.base_url + ' · token ' + (c.token_fingerprint || 'none') + ' · webhook ' + c.webhook_fingerprint + ' · ' + health)),
          h('label', {}, h('input', { type: 'checkbox', 'aria-label': 'Allow write actions for ' + c.name, checked: c.write_enabled, disabled: c.disconnected, onchange: async e => { const d = await api('PUT', '/api/v1/git/connections/' + c.id + '/options', { write_enabled: e.target.checked }); if (d.status !== 200) { toast(errText(d)); e.target.checked = !e.target.checked; } } }), ' Write actions'),
          btn('Check', 'Check connection ' + c.name, async () => { const d = await api('POST', '/api/v1/git/connections/' + c.id + '/check'); toast(d.status === 200 ? 'Connection ' + (d.data.health || 'checked') : errText(d)); loadConns(); }),
          btn('Disconnect', 'Disconnect ' + c.name, async () => { const d = await api('DELETE', '/api/v1/git/connections/' + c.id + '?mode=keep_read_only'); if (d.status === 200) loadConns(); else toast(errText(d)); })));
      }
    }
    async function loadRepos() {
      clear(repos);
      const r = await api('GET', '/api/v1/git/repos');
      if (r.status !== 200) { repos.append(row(errText(r))); return; }
      if (!r.data.length) repos.append(none('No repository is mapped.'));
      for (const x of r.data) repos.append(row(h('span', { class: 'sum' }, h('strong', {}, x.full_name), ' ', h('span', { class: 'muted' }, 'projects ' + (x.projects || []).join(', ') + (x.last_sync_at ? ' · synced ' + new Date(x.last_sync_at).toLocaleString() : ' · not synced'))), x.sync_error ? h('span', { class: 'err' }, x.sync_error) : null,
        btn('Sync', 'Sync ' + x.full_name, async () => { const d = await api('POST', '/api/v1/git/repos/' + x.id + '/sync'); toast(d.status === 200 ? d.data.events + ' event(s) applied' : errText(d)); loadRepos(); })));
    }
    async function loadVis() {
      clear(vis);
      const r = await api('GET', '/api/v1/git/visibility');
      if (r.status !== 200) { vis.append(h('div', { class: 'muted' }, errText(r))); return; }
      const sel = h('select', { 'aria-label': 'Who sees development data' }, h('option', { value: 'default' }, 'Anyone who can see the issue'), h('option', { value: 'restricted' }, 'Only people with access to a private repository'));
      sel.value = r.data.mode;
      sel.addEventListener('change', async () => { const d = await api('PUT', '/api/v1/git/visibility', { mode: sel.value }); toast(d.status === 200 ? 'Visibility saved' : errText(d)); loadVis(); });
      vis.append(sel);
      if (r.data.mode === 'restricted') for (const x of r.data.repos || []) {
        const state = x.private === null || x.private === undefined ? 'privacy unknown (treated as private)' : x.private ? 'private' : 'public';
        const set = async v => { const d = await api('PUT', '/api/v1/git/repos/' + x.id + '/privacy', { private: v }); if (d.status === 200) loadVis(); else toast(errText(d)); };
        vis.append(row(h('span', { class: 'sum' }, h('strong', {}, x.full_name), ' ', h('span', { class: 'muted' }, state + ' · ' + x.members + ' with access' + (x.access_list_synced_at ? ' · list read ' + new Date(x.access_list_synced_at).toLocaleString() : ' · list not read'))),
          btn('Treat as private', 'Treat ' + x.full_name + ' as private', () => set(true)), btn('Treat as public', 'Treat ' + x.full_name + ' as public', () => set(false)), btn('Use the provider’s answer', 'Use the provider answer for ' + x.full_name, () => set(null))));
      }
    }
    async function loadIds() {
      clear(ids);
      const r = await api('GET', '/api/v1/git/identities');
      if (r.status !== 200) { ids.append(row(errText(r))); return; }
      if (!r.data.length) ids.append(none('No provider account is linked to a person.'));
      for (const x of r.data) ids.append(row(h('span', { class: 'sum' }, h('strong', {}, x.login), ' ', h('span', { class: 'muted' }, x.provider + ' → ' + x.user)),
        btn('Unlink', 'Unlink ' + x.login, async () => { const d = await api('DELETE', '/api/v1/git/identities/' + x.id); if (d.status === 200) loadIds(); else toast(errText(d)); })));
    }
    async function loadRules() {
      clear(rules); clear(runs);
      if (!projSel.value) { rules.append(none('No project.')); return; }
      const k = projSel.value;
      const sc = await api('GET', '/api/v1/projects/' + k + '/git/smart-commits');
      if (sc.status === 200) commits.value = sc.data.mode;
      const r = await api('GET', '/api/v1/projects/' + k + '/git/rules');
      if (r.status !== 200) { rules.append(row(errText(r))); return; }
      if (!r.data.length) rules.append(none('No rule in this project.'));
      for (const x of r.data) {
        const mode = h('select', { 'aria-label': 'Mode of rule ' + x.name }, h('option', { value: 'off' }, 'Off'), h('option', { value: 'dry_run' }, 'Dry run'), h('option', { value: 'on' }, 'On'));
        mode.value = x.mode;
        mode.addEventListener('change', async () => { const d = await api('PUT', '/api/v1/projects/' + k + '/git/rules/' + x.id, { ...x, mode: mode.value }); if (d.status !== 200) toast(errText(d)); loadRules(); });
        rules.append(row(h('span', { class: 'sum' }, h('strong', {}, x.name), ' ', h('span', { class: 'muted' }, 'when ' + x.trigger.replace('_', ' ') + (x.transition ? ' → ' + x.transition : '') + (x.comment ? ' · comments' : ''))), mode,
          btn('Delete', 'Delete rule ' + x.name, async () => { const d = await api('DELETE', '/api/v1/projects/' + k + '/git/rules/' + x.id); if (d.status === 200) loadRules(); else toast(errText(d)); })));
      }
      const q = await api('GET', '/api/v1/projects/' + k + '/git/rule-runs?limit=20');
      if (q.status === 200) {
        if (!q.data.length) runs.append(none('No run yet.'));
        for (const x of q.data) runs.append(row(h('span', { class: 'sum' }, h('strong', {}, x.rule), ' ', x.issue, ' ', h('span', { class: 'muted' }, x.outcome + (x.detail ? ': ' + x.detail : '') + ' · ' + new Date(x.at).toLocaleString()))));
      }
    }
    commits.addEventListener('change', async () => { const d = await api('PUT', '/api/v1/projects/' + projSel.value + '/git/smart-commits', { mode: commits.value }); toast(d.status === 200 ? 'Smart commits saved' : errText(d)); });
    projSel.addEventListener('change', loadRules);
    const cf = {};
    for (const [k, label, type, ph] of [['name', 'Connection name', 'text', 'GitHub'], ['base_url', 'Base URL', 'text', 'https://api.github.com'], ['token', 'Access token', 'password', 'token']]) cf[k] = h('input', { type, 'aria-label': label, placeholder: ph, autocomplete: 'off' });
    const prov = h('select', { 'aria-label': 'Provider' }, h('option', { value: 'github' }, 'GitHub'), h('option', { value: 'gitlab' }, 'GitLab'));
    const rf = { conn: h('select', { 'aria-label': 'Connection for the repository' }), name: h('input', { type: 'text', 'aria-label': 'Repository', placeholder: 'owner/name' }), proj: h('input', { type: 'text', 'aria-label': 'Projects to link', placeholder: 'ABC, DEF' }) };
    const lf = { provider: h('select', { 'aria-label': 'Account provider' }, h('option', { value: 'github' }, 'GitHub'), h('option', { value: 'gitlab' }, 'GitLab')), login: h('input', { type: 'text', 'aria-label': 'Provider account', placeholder: 'octocat' }), user: h('select', { 'aria-label': 'Person' }) };
    const nr = { name: h('input', { type: 'text', 'aria-label': 'Rule name', placeholder: 'Move to In Progress on a branch' }), trigger: h('select', { 'aria-label': 'Trigger' }, ...[['branch_created', 'A branch is created'], ['pr_opened', 'A pull request is opened'], ['pr_merged', 'A pull request is merged'], ['build_failed', 'A build fails']].map(([v, t]) => h('option', { value: v }, t))), transition: h('input', { type: 'text', 'aria-label': 'Transition', placeholder: 'Start progress' }), comment: h('input', { type: 'text', 'aria-label': 'Comment', placeholder: 'Comment (optional)' }) };
    app.append(topbar(), adminTabs('git'), h('div', { class: 'page' }, h('h2', {}, 'Git'),
      h('p', { class: 'muted' }, 'Pull requests, branches, builds and deployments are linked to issues by their keys. Tasklane stores no source and no message bodies, and the provider is read only unless write actions are allowed for a connection.'),
      h('h3', {}, 'Connections'), conns,
      h('div', { class: 'chips', style: 'margin:8px 0' }, cf.name, prov, cf.base_url, cf.token, h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/git/connections', { name: cf.name.value, provider: prov.value, base_url: cf.base_url.value, token: cf.token.value });
        if (r.status === 201) { msg.textContent = ''; for (const k in cf) cf[k].value = ''; loadConns(); refreshConnSel(); const close = modal(h('div', {}, h('h2', {}, 'Webhook'), h('p', {}, 'Send the provider’s webhooks to this address with this secret. The secret is shown once.'), h('pre', { class: 'code', 'aria-label': 'Webhook address' }, r.data.webhook_url), h('pre', { class: 'code', 'aria-label': 'Webhook secret' }, r.data.webhook_secret), h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Done')))); }
        else msg.textContent = errText(r);
      } }, 'Add connection')), msg,
      h('h3', { style: 'margin-top:16px' }, 'Repositories'), repos,
      h('div', { class: 'chips', style: 'margin:8px 0' }, rf.conn, rf.name, rf.proj, h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/git/connections/' + rf.conn.value + '/repos', { full_name: rf.name.value, projects: rf.proj.value.split(/[\s,]+/).filter(Boolean) });
        if (r.status === 201) { rf.name.value = ''; rf.proj.value = ''; rmsg.textContent = ''; loadRepos(); } else rmsg.textContent = errText(r);
      } }, 'Map repository')), rmsg,
      h('h3', { style: 'margin-top:16px' }, 'Who sees development data'), h('p', { class: 'muted' }, 'In the restricted setting a person sees the code of a private repository only when their linked account has access to it. Until a repository’s privacy and access list are read, it counts as private.'), vis,
      h('h3', { style: 'margin-top:16px' }, 'Linked accounts'), h('p', { class: 'muted' }, 'A commit can act as a person (smart commits) and show as theirs only through an account you link here, or an address the provider vouches for.'), ids,
      h('div', { class: 'chips', style: 'margin:8px 0' }, lf.provider, lf.login, lf.user, h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/git/identities', { provider: lf.provider.value, login: lf.login.value, user: lf.user.value });
        if (r.status === 201) { lf.login.value = ''; imsg.textContent = ''; loadIds(); } else imsg.textContent = errText(r);
      } }, 'Link account')), imsg,
      h('div', { class: 'chips', style: 'margin-top:16px' }, h('h3', { style: 'margin:0' }, 'Project'), projSel, h('label', {}, 'Smart commits ', commits)),
      h('h3', {}, 'Rules'), h('p', { class: 'muted' }, 'A rule acts on a live event once per object and issue. A dry run records what would happen and changes nothing.'), rules,
      h('div', { class: 'chips', style: 'margin:8px 0' }, nr.name, nr.trigger, nr.transition, nr.comment, h('button', { class: 'btn', onclick: async () => {
        const r = await api('POST', '/api/v1/projects/' + projSel.value + '/git/rules', { name: nr.name.value, trigger: nr.trigger.value, transition: nr.transition.value, comment: nr.comment.value, mode: 'dry_run' });
        if (r.status === 201) { nr.name.value = ''; nr.transition.value = ''; nr.comment.value = ''; umsg.textContent = ''; loadRules(); } else umsg.textContent = errText(r);
      } }, 'Add rule (dry run)')), umsg,
      h('h3', {}, 'Recent runs'), runs));
    async function refreshConnSel() {
      const r = await api('GET', '/api/v1/git/connections'); clear(rf.conn);
      if (r.status === 200) for (const c of r.data) if (!c.disconnected) rf.conn.append(h('option', { value: c.id }, c.name));
    }
    const [pr, us] = await Promise.all([api('GET', '/api/v1/projects'), api('GET', '/api/v1/users')]);
    if (pr.status === 200) for (const p of pr.data) projSel.append(h('option', { value: p.key }, p.key + ' ' + p.name));
    if (us.status === 200) for (const u of us.data) lf.user.append(h('option', { value: u.id }, u.name || u.email));
    await Promise.all([loadConns(), loadRepos(), loadVis(), loadIds(), loadRules(), refreshConnSel()]);
  }

  A.EXT.routes.push({ re: /^#\/admin\/git\/?$/, run: () => gitView() });
  A.EXT.routes.push({ re: /^#\/admin\/email\/?$/, run: () => emailView() });
  A.EXT.routes.push({ re: /^#\/admin\/workflows\/([0-9a-f-]{36})/, run: m => editorView(m[1]) });
  A.EXT.routes.push({ re: /^#\/admin\/workflows\/?$/, run: () => listView() });
  A.EXT.routes.push({ re: /^#\/admin\/audit\/?$/, run: () => auditView() });
  A.EXT.routes.push({ re: /^#\/admin\/identity\/?$/, run: () => identityView() });
  A.EXT.routes.push({ re: /^#\/admin\/automation(?:\/([A-Za-z0-9]+))?\/?$/, run: m => automationView(m[1] ? m[1].toUpperCase() : '') });
})();
