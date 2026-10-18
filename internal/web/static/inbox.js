// The in-app inbox: notifications, read state, preferences. Plugs into the app shell through window.TLApp.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const KINDS = [['assigned', 'Assigned to me'], ['mention', 'Mentions'], ['comment', 'Comments'], ['status', 'Status changes'], ['edit', 'Description edits'], ['created', 'Created for me']];

  async function inboxView(state) {
    const app = clear(document.getElementById('app'));
    const list = h('div', { class: 'card', role: 'list' }), more = h('button', { class: 'btn', hidden: true }, 'Load more');
    const tab = (k, label) => h('a', { href: '#/inbox' + (k ? '/' + k : ''), class: 'tab', 'aria-current': (state || '') === k ? 'page' : null }, label);
    let before = 0, before_loading = false;
    async function load(reset) {
      before_loading = true;
      try { await loadPage(reset); } finally { before_loading = false; }
    }
    async function loadPage(reset) {
      if (reset) { clear(list); before = 0; }
      const r = await api('GET', '/api/v1/notifications?limit=50' + (state ? '&state=' + state : '') + (before ? '&before=' + before : ''));
      if (r.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, errText(r))); return; }
      for (const n of r.data) {
        const set = async st => { const x = await api('POST', '/api/v1/notifications/' + n.id + '/state', { state: st }); if (x.status === 200) { await load(true); A.bell && A.bell(); } else toast(errText(x)); };
        list.append(h('div', { class: 'row', role: 'listitem' },
          h('span', { class: n.state === 'unread' ? 'sum' : 'sum muted', style: n.state === 'unread' ? 'font-weight:600' : '' },
            h('a', { href: '#/issue/' + n.issue }, n.issue), ' ', n.what, n.body ? h('span', { class: 'muted' }, ' — ' + n.body.slice(0, 140)) : null),
          h('span', { class: 'muted' }, window.L && L.relTime ? L.relTime(n.at) : new Date(n.at).toLocaleString()),
          n.state !== 'read' ? h('button', { class: 'btn ghost', 'aria-label': 'Mark ' + n.issue + ' notification read', onclick: () => set('read') }, 'Read') : h('button', { class: 'btn ghost', 'aria-label': 'Mark ' + n.issue + ' notification unread', onclick: () => set('unread') }, 'Unread'),
          n.state !== 'archived' ? h('button', { class: 'btn ghost', 'aria-label': 'Archive ' + n.issue + ' notification', onclick: () => set('archived') }, 'Archive') : null));
        before = n.id;
      }
      more.hidden = r.data.length < 50;
      if (reset && !r.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, state === 'archived' ? 'Nothing archived.' : 'You are all caught up.')));
    }
    more.addEventListener('click', () => load(false));
    app.append(topbar(), h('nav', { class: 'tabs', 'aria-label': 'Inbox views' }, h('span', { class: 'tabs-title' }, 'Inbox'), tab('unread', 'Unread'), tab('', 'All'), tab('archived', 'Archived')),
      h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Notifications'), h('span', { class: 'grow' }),
        h('button', { class: 'btn', onclick: async () => { const r = await api('POST', '/api/v1/notifications/read-all'); if (r.status === 200) { toast(r.data.marked + ' marked read'); await load(true); A.bell && A.bell(); } } }, 'Mark all read'),
        h('button', { class: 'btn', onclick: prefsDialog }, 'Preferences')), list, h('div', { style: 'margin-top:8px' }, more)));
    await load(true);
    // new notifications appear while the page is open (the notifier writes them within a second or two of the event)
    A.timer(() => { if (!document.querySelector('.scrim') && document.getElementById('app').contains(list) && !before_loading) { load(true); A.bell && A.bell(); } }, 5000);
  }

  async function prefsDialog() {
    const r = await api('GET', '/api/v1/me/notification-prefs');
    if (r.status !== 200) { toast(errText(r)); return; }
    const p = r.data;
    const cb = (label, on) => { const i = h('input', { type: 'checkbox', checked: !!on }); return { i, el: h('label', { style: 'display:flex;gap:6px;align-items:center' }, i, label) }; };
    const watch = cb('Watch issues I report, am assigned or comment on', p.auto_watch), inbox = cb('In-app notifications', p.inbox), email = cb('Email notifications', p.email);
    const kinds = KINDS.map(([k, label]) => Object.assign(cb(label, !(p.muted || []).includes(k)), { k }));
    const qs = h('input', { type: 'time', value: p.quiet_start || '', 'aria-label': 'Quiet hours start' }), qe = h('input', { type: 'time', value: p.quiet_end || '', 'aria-label': 'Quiet hours end' });
    const tz = h('input', { type: 'text', value: p.timezone || '', placeholder: 'Europe/Berlin', 'aria-label': 'Time zone for quiet hours' });
    const msg = h('div', { class: 'err', role: 'alert' });
    const close = modal(h('div', {}, h('h2', {}, 'Notification preferences'), p.email_bouncing ? h('p', { class: 'err', role: 'status' }, 'Email to your address is being held back because it bounced. Notifications still arrive here in the inbox. Ask an administrator to clear it once the address works.') : null, watch.el, inbox.el, email.el,
      h('strong', { style: 'display:block;margin-top:8px' }, 'Tell me about'), ...kinds.map(x => x.el),
      h('strong', { style: 'display:block;margin-top:8px' }, 'Hold email during quiet hours'), h('div', { class: 'chips' }, qs, ' to ', qe, tz), msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
        const body = { auto_watch: watch.i.checked, inbox: inbox.i.checked, email: email.i.checked, muted: kinds.filter(x => !x.i.checked).map(x => x.k), quiet_start: qs.value, quiet_end: qe.value, timezone: tz.value.trim() };
        const x = await api('PUT', '/api/v1/me/notification-prefs', body);
        if (x.status === 200) { close(); toast('Preferences saved'); } else msg.textContent = errText(x);
      } }, 'Save'))));
  }

  A.EXT.routes.push({ re: /^#\/inbox(?:\/(unread|archived))?\/?$/, run: m => inboxView(m[1] || '') });
  A.prefsDialog = prefsDialog;
})();
