// My approvals: what is waiting for my decision, across issues.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar } = A;
  async function view() {
    const app = clear(document.getElementById('app'));
    const list = h('div', { class: 'list', role: 'list', 'aria-label': 'Waiting for your decision' });
    async function load() {
      clear(list);
      const r = await api('GET', '/api/v1/approvals');
      if (r.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, (r.data && r.data.error) || 'Something went wrong')); return; }
      if (!r.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'Nothing is waiting for you.')));
      for (const ap of r.data) {
        const c = h('input', { type: 'text', 'aria-label': 'Comment on ' + ap.issue, placeholder: 'Comment (optional)', style: 'min-width:200px' });
        const act = d => async () => { const x = await api('POST', '/api/v1/approvals/' + ap.id + '/decision', { decision: d, comment: c.value }); if (x.status === 200) load(); else toast((x.data && x.data.error) || 'Could not record the decision'); };
        list.append(h('div', { class: 'row', role: 'listitem' },
          h('span', { class: 'sum' }, h('a', { href: '#/issue/' + ap.issue }, ap.issue), ' ' + ap.summary, ap.requested_by ? h('span', { class: 'muted' }, ' · asked by ' + ap.requested_by.name) : null, ap.overdue ? h('span', { class: 'badge', style: 'margin-left:6px' }, 'Overdue') : null),
          c, h('button', { class: 'btn primary', 'aria-label': 'Approve ' + ap.issue, onclick: act('approve') }, 'Approve'), h('button', { class: 'btn', 'aria-label': 'Reject ' + ap.issue, onclick: act('reject') }, 'Reject')));
      }
    }
    app.append(topbar(), h('div', { class: 'page' }, h('h2', {}, 'Approvals'), list));
    await load();
  }
  A.EXT.routes.push({ re: /^#\/approvals\/?$/, run: () => view() });
})();
