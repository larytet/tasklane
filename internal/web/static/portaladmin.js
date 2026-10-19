// Portal settings for a project: whether the portal is open, its texts, who may sign in, and the request types (forms) it offers.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const lines = t => t.split(/[\n,]+/).map(x => x.trim()).filter(Boolean);

  async function view(key) {
    const app = clear(document.getElementById('app'));
    key = key.toUpperCase();
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === key }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/portal-admin/' + sel.value; });
    const msg = h('div', { role: 'status' });
    const body = h('div', {});
    app.append(topbar(), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Customer portal'), sel, h('a', { href: '/portal/' + key, target: '_blank', rel: 'noopener' }, 'Open the portal')), msg, body));
    const r = await api('GET', '/api/v1/projects/' + key + '/portal');
    if (r.status !== 200) { body.append(h('p', {}, r.status === 403 ? 'Only a project administrator can change the portal.' : r.status === 404 ? 'That project does not exist, or you cannot see it.' : errText(r))); return; }
    const p = r.data;
    const enabled = h('input', { type: 'checkbox', id: 'pe-on', checked: p.enabled });
    const welcome = h('textarea', { id: 'pe-w', rows: 2, 'aria-label': 'Welcome text' }); welcome.value = p.welcome;
    const privacy = h('textarea', { id: 'pe-p', rows: 3, 'aria-label': 'Privacy notice' }); privacy.value = p.privacy_notice;
    const consent = h('textarea', { id: 'pe-c', rows: 2, 'aria-label': 'Consent text' }); consent.value = p.consent_text;
    const allow = h('textarea', { id: 'pe-a', rows: 2, 'aria-label': 'Allowed addresses and domains' }); allow.value = (p.allow || []).join('\n');
    const block = h('textarea', { id: 'pe-b', rows: 2, 'aria-label': 'Blocked addresses and domains' }); block.value = (p.block || []).join('\n');
    const af = h('input', { type: 'checkbox', id: 'pe-af', checked: p.attach_enabled });
    const amb = h('input', { type: 'number', id: 'pe-amb', min: 1, max: 100, value: p.attach_max_mb, 'aria-label': 'Largest file in MB' });
    const afn = h('input', { type: 'number', id: 'pe-afn', min: 1, max: 20, value: p.attach_max_files, 'aria-label': 'Most files per request' });
    const cap = h('input', { type: 'number', id: 'pe-cap', min: 0, max: 24, value: p.captcha_bits, 'aria-label': 'Sign-in check difficulty' });
    const acl = ['image', 'document', 'log'].map(c => h('label', {}, h('input', { type: 'checkbox', 'data-class': c, checked: (p.attach_classes || []).includes(c) }), ' ' + ({ image: 'Images', document: 'Documents', log: 'Text and logs' }[c])));
    body.append(h('h3', {}, 'Settings'),
      h('label', { for: 'pe-on' }, enabled, ' The portal is open'),
      h('label', { for: 'pe-w' }, 'Welcome text'), welcome, h('label', { for: 'pe-p' }, 'Privacy notice'), privacy, h('label', { for: 'pe-c' }, 'Consent text'), consent,
      h('label', { for: 'pe-a' }, 'Only these may sign in (addresses or @domains, one per line; empty allows everyone)'), allow,
      h('label', { for: 'pe-b' }, 'These may never sign in'), block,
      h('label', { for: 'pe-cap' }, 'Sign-in check (0 is off; 16 costs a visitor\'s browser about a second; at most 24)'), cap,
      h('h4', {}, 'Files from customers'),
      h('label', { for: 'pe-af' }, af, ' Customers may add files to their requests'),
      h('label', { for: 'pe-amb' }, 'Largest file (MB)'), amb, h('label', { for: 'pe-afn' }, 'Most files per request'), afn,
      h('div', { class: 'chips' }, acl), h('p', { class: 'muted' }, 'Archives and video are never accepted. Every file is checked on what it really is, not on its name, and flagged files are refused.'),
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: async () => {
        const x = await api('PUT', '/api/v1/projects/' + key + '/portal', { enabled: enabled.checked, welcome: welcome.value, privacy_notice: privacy.value, consent_text: consent.value, allow: lines(allow.value), block: lines(block.value), captcha_bits: +cap.value, attach_enabled: af.checked, attach_max_mb: +amb.value, attach_max_files: +afn.value, attach_classes: acl.map(l => l.querySelector('input')).filter(i => i.checked).map(i => i.dataset.class) });
        msg.className = x.status === 200 ? 'muted' : 'err'; msg.textContent = x.status === 200 ? 'Saved.' : errText(x);
      } }, 'Save settings')));
    // customers as data subjects: find, export, erase, promote (administrators of the organization)
    const cq = h('input', { type: 'search', 'aria-label': 'Find a customer', placeholder: 'Find a customer by address or name' });
    const clist = h('div', { class: 'list', role: 'list', 'aria-label': 'Customers' });
    const cmsg = h('div', { role: 'status' });
    async function findCustomers() {
      clear(clist);
      const x = await api('GET', '/api/v1/admin/customers?limit=20&q=' + encodeURIComponent(cq.value));
      if (x.status !== 200) { clist.append(h('div', { class: 'row', role: 'listitem' }, x.status === 403 ? 'Only administrators of the organization manage customers.' : errText(x))); return; }
      if (!x.data.length) clist.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No customers found.')));
      for (const c of x.data) {
        const gone = !!c.erased;
        clist.append(h('div', { class: 'row', role: 'listitem' },
          h('span', { class: 'sum' }, h('strong', {}, c.email), ' ', h('span', { class: 'muted' }, [c.name, c.company, c.verified ? '' : 'never signed in', c.blocked && !gone ? 'blocked' : '', gone ? 'erased' : '', c.promoted_to ? 'staff' : ''].filter(Boolean).join(' · '))),
          h('button', { class: 'btn ghost', disabled: gone, 'aria-label': 'Export data of ' + c.email, onclick: async () => {
            const r = await fetch('/api/v1/admin/customers/' + c.id + '/personal-data', { credentials: 'same-origin' });
            if (!r.ok) { cmsg.className = 'err'; cmsg.textContent = 'Could not export.'; return; }
            const url = URL.createObjectURL(await r.blob()); const a = h('a', { href: url, download: 'customer-data.json' }); document.body.append(a); a.click(); a.remove(); URL.revokeObjectURL(url);
          } }, 'Export'),
          h('button', { class: 'btn ghost', disabled: gone || !!c.promoted_to, 'aria-label': 'Promote ' + c.email + ' to staff', onclick: () => promote(c) }, 'Make staff'),
          h('button', { class: 'btn ghost', disabled: gone, 'aria-label': 'Erase ' + c.email, onclick: () => erase(c) }, 'Erase')));
      }
    }
    function erase(c) {
      const content = h('input', { type: 'checkbox', id: 'ec-del' });
      let close;
      close = modal(h('div', {}, h('h2', {}, 'Erase ' + c.email), h('p', {}, 'Removes the person from the portal, their sign-ins and mail. Their requests stay as work records without their name or address. Requests under legal hold stop the erase.'),
        h('label', { for: 'ec-del' }, content, ' Also delete what they wrote'),
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('POST', '/api/v1/admin/customers/' + c.id + '/erase', { delete_content: content.checked, scrub_mentions: true });
          close(); if (x.status === 200) { cmsg.className = 'muted'; cmsg.textContent = 'Erased.'; findCustomers(); } else { cmsg.className = 'err'; cmsg.textContent = errText(x); }
        } }, 'Erase'))));
    }
    function promote(c) {
      const name = h('input', { type: 'text', id: 'pm-n', value: c.name || '' }), pw = h('input', { type: 'password', id: 'pm-p', autocomplete: 'new-password' });
      const err = h('div', { role: 'alert', class: 'err' });
      let close;
      close = modal(h('div', {}, h('h2', {}, 'Make ' + c.email + ' a member of staff'), h('p', {}, 'Creates a staff account with this address. Their requests and comments stay as they are, and they can still use the portal.'),
        h('label', { for: 'pm-n' }, 'Name'), name, h('label', { for: 'pm-p' }, 'Initial password'), pw, err,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('POST', '/api/v1/admin/customers/' + c.id + '/promote', { name: name.value, password: pw.value });
          if (x.status === 201) { close(); cmsg.className = 'muted'; cmsg.textContent = 'Staff account made.'; findCustomers(); } else err.textContent = errText(x);
        } }, 'Make staff'))));
    }
    cq.addEventListener('input', () => { clearTimeout(cq._t); cq._t = setTimeout(findCustomers, 300); });
    body.append(h('h3', { style: 'margin-top:16px' }, 'Customers'), cq, clist, cmsg);
    findCustomers();
    // customer organizations: the companies whose people can share requests
    const orgs = h('div', { class: 'list', role: 'list', 'aria-label': 'Customer organizations' });
    const oname = h('input', { type: 'text', 'aria-label': 'Customer organization name', placeholder: 'Acme Ltd', maxlength: 100 });
    const oerr = h('div', { role: 'alert', class: 'err' });
    body.append(h('h3', { style: 'margin-top:16px' }, 'Customer organizations'), orgs, h('div', { class: 'chips' }, oname, h('button', { class: 'btn primary', onclick: async () => {
      oerr.textContent = '';
      const x = await api('POST', '/api/v1/projects/' + key + '/portal/customer-orgs', { name: oname.value });
      if (x.status === 201) { oname.value = ''; loadOrgs(); } else oerr.textContent = errText(x);
    } }, 'Add customer organization')), oerr);
    async function loadOrgs() {
      clear(orgs);
      const t = await api('GET', '/api/v1/projects/' + key + '/portal/customer-orgs');
      if (t.status !== 200) { orgs.append(h('div', { class: 'row', role: 'listitem' }, errText(t))); return; }
      if (!t.data.length) orgs.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'None yet. Customers can share a request with named people without one.')));
      for (const o of t.data) {
        const add = h('input', { type: 'text', 'aria-label': 'Add members to ' + o.name, placeholder: 'ana@acme.example, bo@acme.example', style: 'min-width:260px' });
        orgs.append(h('div', { class: 'row', role: 'listitem', style: 'flex-direction:column;align-items:stretch;gap:4px' },
          h('div', {}, h('strong', {}, o.name), ' ', h('span', { class: 'muted' }, o.members.length + ' member(s)'), h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + o.name, onclick: async () => { const x = await api('DELETE', '/api/v1/projects/' + key + '/portal/customer-orgs/' + o.id); if (x.status === 200) loadOrgs(); else toast(errText(x)); } }, 'Delete')),
          h('ul', { style: 'margin:0;padding-left:18px' }, o.members.map(m => h('li', {}, m, ' ', h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + m + ' from ' + o.name, onclick: async () => { const x = await api('PUT', '/api/v1/projects/' + key + '/portal/customer-orgs/' + o.id + '/members', { remove: [m] }); if (x.status === 200) loadOrgs(); else toast(errText(x)); } }, 'Remove')))),
          h('div', { class: 'chips' }, add, h('button', { class: 'btn', onclick: async () => { const x = await api('PUT', '/api/v1/projects/' + key + '/portal/customer-orgs/' + o.id + '/members', { add: lines(add.value) }); if (x.status === 200) loadOrgs(); else toast(errText(x)); } }, 'Add members'))));
      }
    }
    loadOrgs();
    // request types
    const list = h('div', { class: 'list', role: 'list', 'aria-label': 'Request types' });
    body.append(h('h3', { style: 'margin-top:16px' }, 'Request types'), list);
    async function loadTypes() {
      clear(list);
      const t = await api('GET', '/api/v1/projects/' + key + '/request-types');
      if (t.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, errText(t))); return; }
      if (!t.data.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No request types yet. Customers see nothing to choose from.')));
      for (const rt of t.data) {
        list.append(h('div', { class: 'row', role: 'listitem' },
          h('span', { class: 'sum' }, h('strong', {}, rt.name), ' ', h('span', { class: 'muted' }, 'becomes ' + rt.issue_type + (rt.fields.length ? ' · asks for ' + rt.fields.map(f => f.label).join(', ') : '') + (rt.sla && (rt.sla.first_response_minutes || rt.sla.resolution_minutes) ? ' · goals: ' + [rt.sla.first_response_minutes ? 'reply in ' + rt.sla.first_response_minutes + ' min' : '', rt.sla.resolution_minutes ? 'resolve in ' + rt.sla.resolution_minutes + ' min' : ''].filter(Boolean).join(', ') + (rt.sla.calendar ? ' (working time, ' + rt.sla.calendar.time_zone + ')' : '') : '') + (rt.visible ? '' : ' · hidden'))),
          h('button', { class: 'btn ghost', 'aria-label': (rt.visible ? 'Hide ' : 'Show ') + rt.name, onclick: async () => { const x = await api('PUT', '/api/v1/request-types/' + rt.id, { visible: !rt.visible }); if (x.status === 200) loadTypes(); else toast(errText(x)); } }, rt.visible ? 'Hide' : 'Show'),
          h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + rt.name, onclick: async () => { const x = await api('DELETE', '/api/v1/request-types/' + rt.id); if (x.status === 200) loadTypes(); else toast(errText(x)); } }, 'Delete')));
      }
    }
    const name = h('input', { type: 'text', 'aria-label': 'Request type name', placeholder: 'Billing question', maxlength: 100 });
    const type = h('select', { 'aria-label': 'Becomes issue type' }, ['Task', 'Bug', 'Story', 'Epic'].map(x => h('option', {}, x)));
    const desc = h('input', { type: 'text', 'aria-label': 'Description for customers', placeholder: 'Description (optional)', style: 'min-width:220px' });
    const help = h('input', { type: 'text', 'aria-label': 'Help text', placeholder: 'Help shown above the form (optional)', style: 'min-width:220px' });
    const fields = h('input', { type: 'text', 'aria-label': 'Fields the form asks for', placeholder: 'Custom field keys, e.g. customfield_10001, customfield_10002', style: 'min-width:300px' });
    const fr = h('input', { type: 'number', min: 0, 'aria-label': 'First response goal in minutes', placeholder: 'First response (min)', style: 'width:150px' });
    const rs = h('input', { type: 'number', min: 0, 'aria-label': 'Resolution goal in minutes', placeholder: 'Resolution (min)', style: 'width:150px' });
    const tz = h('input', { type: 'text', 'aria-label': 'Working time zone', placeholder: 'Working hours time zone, e.g. Europe/Prague (empty: every minute counts)', style: 'min-width:260px' });
    const st = h('input', { type: 'text', 'aria-label': 'Working day starts', placeholder: '09:00', style: 'width:90px' });
    const ps = h('input', { type: 'text', 'aria-label': 'Statuses that stop the clock', placeholder: 'Statuses that stop the clock, comma separated', style: 'min-width:260px' });
    const err = h('div', { role: 'alert', class: 'err' });
    body.append(h('div', { class: 'chips' }, name, type, desc, help, fields), h('div', { class: 'chips' }, fr, rs, tz, st, ps, h('button', { class: 'btn primary', onclick: async () => {
      err.textContent = '';
      const x = await api('POST', '/api/v1/projects/' + key + '/request-types', { name: name.value, issue_type: type.value, description: desc.value, help_text: help.value, fields: lines(fields.value).map(k => ({ key: k, required: false })),
        sla: { first_response_minutes: Number(fr.value) || 0, resolution_minutes: Number(rs.value) || 0, calendar: tz.value.trim() ? { time_zone: tz.value.trim(), start: st.value.trim() } : null, pause_statuses: lines(ps.value) } });
      if (x.status === 201) { name.value = desc.value = help.value = fields.value = fr.value = rs.value = tz.value = st.value = ps.value = ''; loadTypes(); } else err.textContent = errText(x);
    } }, 'Add request type')), err);
    await loadTypes();
  }
  A.EXT.routes.push({ re: /^#\/portal-admin\/([A-Za-z][A-Za-z0-9]+)\/?$/, run: m => view(m[1]) });
})();
