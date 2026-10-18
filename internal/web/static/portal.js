// The customer portal page: its own small program, with none of the staff app's code. Customers sign in with a code or link sent to their
// address (or a password they chose), and reach nothing else. Everything the server sends is shown as text.
(function () {
  'use strict';
  const root = document.getElementById('portal');
  const key = (location.pathname.split('/')[2] || '').toUpperCase();
  const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
  async function api(method, path, body) {
    const r = await fetch(path, { method, credentials: 'same-origin', headers: H, body: body ? JSON.stringify(body) : undefined });
    let data = null; try { data = await r.json(); } catch (e) { /* no body */ }
    return { status: r.status, data };
  }
  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const k in attrs || {}) { const v = attrs[k]; if (v === null || v === undefined || v === false) continue; if (k.startsWith('on')) el.addEventListener(k.slice(2), v); else el.setAttribute(k, v === true ? '' : v); }
    for (const c of kids.flat()) if (c !== null && c !== undefined && c !== false) el.append(c.nodeType ? c : document.createTextNode(String(c)));
    return el;
  }
  const clear = el => { while (el.firstChild) el.removeChild(el.firstChild); return el; };
  const errText = r => (r.data && r.data.error) || 'Something went wrong';

  async function start() {
    if (!key) { clear(root).append(h('h1', {}, 'Support portal'), h('p', {}, 'Open the address of the portal you were given.')); return; }
    const info = await api('GET', '/api/v1/portal/' + key);
    if (info.status !== 200) { clear(root).append(h('h1', {}, 'Support portal'), h('p', {}, 'This portal does not exist or is not open.')); return; }
    document.title = info.data.name + ' · Support';
    const token = new URLSearchParams(location.search).get('token');
    if (token) {
      history.replaceState(null, '', location.pathname); // the token leaves the address bar at once
      const r = await api('POST', '/api/v1/portal/link', { token });
      if (r.status === 200) return home(info.data, r.data);
      return signIn(info.data, 'That link has been used or has expired. Ask for a new code below.');
    }
    const me = await api('GET', '/api/v1/portal/me');
    if (me.status === 200) return home(info.data, me.data);
    return signIn(info.data);
  }

  function signIn(info, notice) {
    const msg = h('p', { role: 'alert', class: 'err' }, notice || '');
    const email = h('input', { type: 'email', id: 'pe', autocomplete: 'email', required: true, maxlength: 254 });
    // the honeypot: off screen, not reachable by keyboard, never filled by a person
    const trap = h('input', { type: 'text', name: 'website', tabindex: '-1', autocomplete: 'off', 'aria-hidden': 'true', style: 'position:absolute;left:-9999px;width:1px;height:1px' });
    const code = h('input', { type: 'text', id: 'pc', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: 6, pattern: '[0-9]{6}' });
    const codeRow = h('div', { hidden: true }, h('label', { for: 'pc' }, 'The six digit code we sent you'), code, h('button', { class: 'btn primary', type: 'submit', id: 'pverify' }, 'Sign in'));
    const ask = h('button', { class: 'btn primary', type: 'button', id: 'pask' }, 'Email me a code');
    const pw = h('input', { type: 'password', id: 'pp', autocomplete: 'current-password' });
    ask.addEventListener('click', async () => {
      msg.textContent = '';
      const r = await api('POST', '/api/v1/portal/' + key + '/code', { email: email.value, website: trap.value });
      if (r.status === 200) { codeRow.hidden = false; code.focus(); msg.className = 'muted'; msg.textContent = 'If that address can use this portal, a code is on its way.'; }
      else { msg.className = 'err'; msg.textContent = r.status === 429 ? 'Too many tries. Please wait a while and try again.' : errText(r); }
    });
    const form = h('form', { onsubmit: async e => {
      e.preventDefault();
      msg.textContent = ''; msg.className = 'err';
      const r = code.value ? await api('POST', '/api/v1/portal/' + key + '/verify', { email: email.value, code: code.value })
        : await api('POST', '/api/v1/portal/' + key + '/login', { email: email.value, password: pw.value });
      if (r.status === 200 && r.data && r.data.email) return home(info, r.data);
      msg.textContent = r.status === 429 ? 'Too many tries. Please wait a while and try again.' : errText(r);
    } },
      h('label', { for: 'pe' }, 'Your email address'), email, trap, h('div', { class: 'chips' }, ask), codeRow,
      h('details', {}, h('summary', {}, 'I have a password'), h('label', { for: 'pp' }, 'Password'), pw, h('button', { class: 'btn', type: 'submit' }, 'Sign in with a password')),
      msg);
    clear(root).append(h('h1', {}, info.name), info.welcome ? h('p', {}, info.welcome) : null, h('h2', {}, 'Sign in'), form, notices(info));
  }

  function notices(info) {
    return h('footer', { class: 'muted' }, info.consent_text ? h('p', {}, info.consent_text) : null, info.privacy_notice ? h('details', {}, h('summary', {}, 'Privacy notice'), h('p', { style: 'white-space:pre-wrap' }, info.privacy_notice)) : null);
  }

  function home(info, me) {
    const name = h('input', { type: 'text', id: 'pn', value: me.name || '', maxlength: 100 });
    const tz = h('input', { type: 'text', id: 'pz', value: me.timezone || '', placeholder: 'Europe/Prague' });
    const msg = h('p', { role: 'status' });
    const pass = h('input', { type: 'password', id: 'pnp', autocomplete: 'new-password' });
    clear(root).append(
      h('h1', {}, info.name),
      h('p', {}, 'Signed in as ', h('strong', {}, me.email), '. ', h('button', { class: 'btn ghost', onclick: async () => { await api('POST', '/api/v1/portal/logout'); location.reload(); } }, 'Sign out')),
      h('h2', {}, 'Your details'),
      h('form', { onsubmit: async e => {
        e.preventDefault();
        const r = await api('PUT', '/api/v1/portal/me', { name: name.value, language: me.language || '', timezone: tz.value });
        msg.className = r.status === 200 ? 'muted' : 'err'; msg.textContent = r.status === 200 ? 'Saved.' : errText(r);
      } }, h('label', { for: 'pn' }, 'Name'), name, h('label', { for: 'pz' }, 'Time zone'), tz, h('button', { class: 'btn primary', type: 'submit' }, 'Save')),
      h('details', {}, h('summary', {}, me.has_password ? 'Change or remove my password' : 'Set a password'),
        h('label', { for: 'pnp' }, 'New password (leave empty to remove it)'), pass,
        h('button', { class: 'btn', type: 'button', onclick: async () => { const r = await api('PUT', '/api/v1/portal/password', { password: pass.value }); msg.className = r.status === 200 ? 'muted' : 'err'; msg.textContent = r.status === 200 ? 'Password saved.' : errText(r); } }, 'Save password')),
      msg, notices(info));
  }
  start();
})();
