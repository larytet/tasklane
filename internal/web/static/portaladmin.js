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
    body.append(h('h3', {}, 'Settings'),
      h('label', { for: 'pe-on' }, enabled, ' The portal is open'),
      h('label', { for: 'pe-w' }, 'Welcome text'), welcome, h('label', { for: 'pe-p' }, 'Privacy notice'), privacy, h('label', { for: 'pe-c' }, 'Consent text'), consent,
      h('label', { for: 'pe-a' }, 'Only these may sign in (addresses or @domains, one per line; empty allows everyone)'), allow,
      h('label', { for: 'pe-b' }, 'These may never sign in'), block,
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: async () => {
        const x = await api('PUT', '/api/v1/projects/' + key + '/portal', { enabled: enabled.checked, welcome: welcome.value, privacy_notice: privacy.value, consent_text: consent.value, allow: lines(allow.value), block: lines(block.value) });
        msg.className = x.status === 200 ? 'muted' : 'err'; msg.textContent = x.status === 200 ? 'Saved.' : errText(x);
      } }, 'Save settings')));
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
          h('span', { class: 'sum' }, h('strong', {}, rt.name), ' ', h('span', { class: 'muted' }, 'becomes ' + rt.issue_type + (rt.fields.length ? ' · asks for ' + rt.fields.map(f => f.label).join(', ') : '') + (rt.visible ? '' : ' · hidden'))),
          h('button', { class: 'btn ghost', 'aria-label': (rt.visible ? 'Hide ' : 'Show ') + rt.name, onclick: async () => { const x = await api('PUT', '/api/v1/request-types/' + rt.id, { visible: !rt.visible }); if (x.status === 200) loadTypes(); else toast(errText(x)); } }, rt.visible ? 'Hide' : 'Show'),
          h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + rt.name, onclick: async () => { const x = await api('DELETE', '/api/v1/request-types/' + rt.id); if (x.status === 200) loadTypes(); else toast(errText(x)); } }, 'Delete')));
      }
    }
    const name = h('input', { type: 'text', 'aria-label': 'Request type name', placeholder: 'Billing question', maxlength: 100 });
    const type = h('select', { 'aria-label': 'Becomes issue type' }, ['Task', 'Bug', 'Story', 'Epic'].map(x => h('option', {}, x)));
    const desc = h('input', { type: 'text', 'aria-label': 'Description for customers', placeholder: 'Description (optional)', style: 'min-width:220px' });
    const help = h('input', { type: 'text', 'aria-label': 'Help text', placeholder: 'Help shown above the form (optional)', style: 'min-width:220px' });
    const fields = h('input', { type: 'text', 'aria-label': 'Fields the form asks for', placeholder: 'Custom field keys, e.g. customfield_10001, customfield_10002', style: 'min-width:300px' });
    const err = h('div', { role: 'alert', class: 'err' });
    body.append(h('div', { class: 'chips' }, name, type, desc, help, fields, h('button', { class: 'btn primary', onclick: async () => {
      err.textContent = '';
      const x = await api('POST', '/api/v1/projects/' + key + '/request-types', { name: name.value, issue_type: type.value, description: desc.value, help_text: help.value, fields: lines(fields.value).map(k => ({ key: k, required: false })) });
      if (x.status === 201) { name.value = desc.value = help.value = fields.value = ''; loadTypes(); } else err.textContent = errText(x);
    } }, 'Add request type')), err);
    await loadTypes();
  }
  A.EXT.routes.push({ re: /^#\/portal-admin\/([A-Za-z][A-Za-z0-9]+)\/?$/, run: m => view(m[1]) });
})();
