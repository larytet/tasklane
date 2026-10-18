// Queues: a project's named queries with counts, the issues in the selected one, and the project's canned responses.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';

  async function view(key, selected) {
    const app = clear(document.getElementById('app'));
    key = key.toUpperCase();
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === key }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/queues/' + sel.value; });
    const nav = h('ul', { class: 'list', role: 'list', 'aria-label': 'Queues' });
    const out = h('div', { 'aria-live': 'polite' });
    const admin = h('div', { hidden: true });
    app.append(topbar(), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Queues'), sel, h('a', { href: '#/portal-admin/' + key }, 'Customer portal settings')), nav, out, admin));

    async function load() {
      clear(nav);
      const r = await api('GET', '/api/v1/projects/' + key + '/queues');
      if (r.status !== 200) { nav.append(h('li', { class: 'row' }, r.status === 404 ? 'That project does not exist, or you cannot see it.' : errText(r))); return; }
      if (!r.data.length) nav.append(h('li', { class: 'row muted' }, 'No queues yet.'));
      for (const q of r.data) {
        nav.append(h('li', { class: 'row' },
          h('a', { href: '#/queues/' + key + '/' + q.id, 'aria-current': q.id === selected ? 'page' : null }, q.name), ' ',
          q.error ? h('span', { class: 'err', role: 'alert' }, q.error) : h('span', { class: 'badge' }, (q.capped ? '≥' : '') + q.count),
          h('button', { class: 'btn ghost', hidden: !canManage, 'aria-label': 'Delete ' + q.name, onclick: async () => { const x = await api('DELETE', '/api/v1/queues/' + q.id); if (x.status === 200) { if (q.id === selected) location.hash = '#/queues/' + key; else view(key, selected); } else toast(errText(x)); } }, 'Delete')));
      }
    }
    async function issues() {
      clear(out);
      if (!selected) return;
      const r = await api('GET', '/api/v1/queues/' + selected + '/issues?limit=50');
      if (r.status !== 200) { out.append(h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.issues.length) { out.append(h('p', { class: 'muted' }, 'Nothing in this queue.')); return; }
      out.append(h('table', { class: 'dash-table' }, h('thead', {}, h('tr', {}, ...['Key', 'Summary', 'Status', 'Priority', 'Assignee'].map(t => h('th', { scope: 'col' }, t)))),
        h('tbody', {}, ...r.data.issues.map(i => h('tr', {}, h('td', {}, h('a', { href: '#/issue/' + i.key }, i.key)), h('td', {}, i.summary), h('td', {}, i.status), h('td', {}, i.priority), h('td', {}, i.assignee || ''))))),
        h('p', { class: 'muted' }, r.data.issues.length === r.data.total ? r.data.total + ' issue(s)' : 'Showing ' + r.data.issues.length + ' of ' + r.data.total));
    }
    let canManage = false;
    // administrators: add a queue, manage canned responses
    const probe = await api('GET', '/api/v1/projects/' + key + '/portal');
    canManage = probe.status === 200;
    if (canManage) {
      admin.hidden = false;
      const name = h('input', { type: 'text', 'aria-label': 'Queue name', placeholder: 'Waiting for us', maxlength: 100 });
      const tql = h('input', { type: 'text', 'aria-label': 'Queue query', placeholder: 'statusCategory != Done AND assignee is EMPTY', style: 'min-width:300px' });
      const qerr = h('div', { role: 'alert', class: 'err' });
      const cname = h('input', { type: 'text', 'aria-label': 'Canned response name', placeholder: 'Thanks', maxlength: 100 });
      const cbody = h('textarea', { 'aria-label': 'Canned response text', rows: 3, placeholder: 'Hello {{customer}}, thanks for writing about {{issue.key}}.' });
      const clist = h('ul', { class: 'list', 'aria-label': 'Canned responses' });
      const cerr = h('div', { role: 'alert', class: 'err' });
      async function loadCanned() {
        clear(clist);
        const c = await api('GET', '/api/v1/projects/' + key + '/canned-responses');
        for (const x of c.data || []) clist.append(h('li', { class: 'row' }, h('strong', {}, x.name), ' ', h('span', { class: 'muted' }, x.body.slice(0, 80)),
          h('button', { class: 'btn ghost', 'aria-label': 'Delete canned response ' + x.name, onclick: async () => { const d = await api('DELETE', '/api/v1/canned-responses/' + x.id); if (d.status === 200) loadCanned(); else toast(errText(d)); } }, 'Delete')));
      }
      admin.append(h('h3', {}, 'New queue'), h('div', { class: 'chips' }, name, tql, h('button', { class: 'btn primary', onclick: async () => {
        qerr.textContent = '';
        const x = await api('POST', '/api/v1/projects/' + key + '/queues', { name: name.value, tql: tql.value });
        if (x.status === 201) location.hash = '#/queues/' + key + '/' + x.data.id; else qerr.textContent = errText(x);
      } }, 'Add queue')), qerr,
        h('h3', { style: 'margin-top:16px' }, 'Canned responses'), clist, h('div', { class: 'chips' }, cname, cbody, h('button', { class: 'btn primary', onclick: async () => {
          cerr.textContent = '';
          const x = await api('POST', '/api/v1/projects/' + key + '/canned-responses', { name: cname.value, body: cbody.value });
          if (x.status === 201) { cname.value = cbody.value = ''; loadCanned(); } else cerr.textContent = errText(x);
        } }, 'Add canned response')), cerr);
      loadCanned();
    }
    await load();
    await issues();
  }
  A.EXT.routes.push({ re: /^#\/queues\/([A-Za-z][A-Za-z0-9]+)\/([0-9a-f-]{36})\/?$/, run: m => view(m[1], m[2]) });
  A.EXT.routes.push({ re: /^#\/queues\/([A-Za-z][A-Za-z0-9]+)\/?$/, run: m => view(m[1], '') });
  A.EXT.routes.push({ re: /^#\/queues\/?$/, run: () => { location.hash = '#/queues/' + (S.projects[0] ? S.projects[0].key : ''); } });
})();
