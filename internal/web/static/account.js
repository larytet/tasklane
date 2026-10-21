// The account page: two-factor authentication, password, sessions and API tokens.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, b64u } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const when = iso => iso ? new Date(iso).toLocaleString() : 'never';

  // The account's sections (two-factor authentication, passkeys, password, sessions, connected applications and API tokens) live in Settings,
  // in the tabs Security, Sessions and API tokens. This builds them, each loading its own data, and Settings puts them in its tabs.
  function buildAccount() {
    const section = (title, ...kids) => h('section', { class: 'card', style: 'padding:12px;margin:12px 0', 'aria-label': title }, h('h2', { style: 'margin:0 0 8px' }, title), ...kids);

    // ----- two-factor authentication
    const mfaBox = h('div', {});
    async function loadMFA() {
      clear(mfaBox);
      const r = await api('GET', '/api/v1/me/2fa');
      if (r.status !== 200) { mfaBox.append(h('p', { class: 'err' }, errText(r))); return; }
      if (r.data.enabled) {
        mfaBox.append(h('p', {}, 'Two-factor authentication is on. ' + r.data.recovery_codes_left + ' recovery codes are left.'),
          h('button', { class: 'btn', onclick: disableMFA }, 'Turn off two-factor authentication'));
      } else {
        mfaBox.append(h('p', { class: 'muted' }, 'Add a second step to signing in with an authenticator app.'), h('button', { class: 'btn primary', onclick: enrolMFA }, 'Set up two-factor authentication'));
      }
    }
    async function enrolMFA() {
      const r = await api('POST', '/api/v1/me/2fa/setup');
      if (r.status !== 200) { toast(errText(r)); return; }
      const code = h('input', { type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code', 'aria-label': 'Code from the app' }), msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Set up two-factor authentication'),
        h('p', {}, r.data.qr ? 'Scan this code with your authenticator app, or add an account with the key below, then enter the code it shows.' : 'In your authenticator app add an account with this key, then enter the code it shows.'),
        r.data.qr ? h('p', {}, h('img', { src: r.data.qr, alt: 'QR code for your authenticator app', width: 200, height: 200, style: 'image-rendering:pixelated' })) : null,
        h('p', {}, h('code', { 'aria-label': 'Setup key', style: 'user-select:all' }, r.data.secret)), h('p', { class: 'muted' }, r.data.otpauth), code, msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('POST', '/api/v1/me/2fa/enable', { code: code.value });
          if (x.status !== 200) { msg.textContent = errText(x); return; }
          close(); recoveryDialog(x.data.recovery_codes); loadMFA();
        } }, 'Turn on'))));
      code.focus();
    }
    function recoveryDialog(codes) {
      const close = modal(h('div', {}, h('h2', {}, 'Your recovery codes'), h('p', {}, 'Keep these somewhere safe. Each works once if you lose your device. They are not shown again.'),
        h('pre', { 'aria-label': 'Recovery codes', style: 'user-select:all' }, codes.join('\n')), h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'I have saved them'))));
    }
    function disableMFA() {
      const pw = h('input', { type: 'password', autocomplete: 'current-password', 'aria-label': 'Your password' }), msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Turn off two-factor authentication'), h('p', {}, 'Enter your password to confirm.'), pw, msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('DELETE', '/api/v1/me/2fa', { password: pw.value });
          if (x.status === 200) { close(); toast('Two-factor authentication is off'); loadMFA(); } else msg.textContent = x.status === 403 ? 'That password is not right.' : errText(x);
        } }, 'Turn off'))));
      pw.focus();
    }

    // ----- passkeys and security keys
    const keysBox = h('div', {});
    async function loadKeys() {
      clear(keysBox);
      const r = await api('GET', '/api/v1/me/passkeys');
      if (r.status !== 200) { keysBox.append(h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.available || !window.PublicKeyCredential) {
        keysBox.append(h('p', { class: 'muted' }, !r.data.available ? 'Passkeys need Tasklane to be reached by a host name, not an IP address, and the address Tasklane is set to use to be known (base_url).' : 'This browser does not support passkeys.'));
        if (!r.data.keys.length) return;
      }
      if (!r.data.keys.length) keysBox.append(h('p', { class: 'muted' }, 'No passkey or security key is registered.'));
      else {
        const list = h('div', { class: 'list', role: 'list', 'aria-label': 'Passkeys' }); // a list role only where there are items
        for (const k of r.data.keys) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, k.name), ' ', h('span', { class: 'muted' }, 'added ' + when(k.created_at) + ' · last used ' + when(k.last_used_at))),
          h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + k.name, onclick: () => removeKey(k) }, 'Remove')));
        keysBox.append(list);
      }
      if (r.data.available && window.PublicKeyCredential) keysBox.append(h('div', { class: 'chips', style: 'margin-top:8px' }, h('button', { class: 'btn primary', onclick: addKey }, 'Add a passkey or security key')));
    }
    function addKey() {
      const name = h('input', { type: 'text', 'aria-label': 'Name for the key', placeholder: 'Laptop, phone, YubiKey', maxlength: 60 }), pw = h('input', { type: 'password', autocomplete: 'current-password', 'aria-label': 'Your password' }), msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Add a passkey or security key'), h('p', {}, 'Name it and enter your password, then follow your browser\u2019s prompt.'), name, pw, msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          msg.textContent = '';
          const o = await api('POST', '/api/v1/me/passkeys/options', { password: pw.value });
          if (o.status !== 200) { msg.textContent = o.status === 403 ? 'That password is not right.' : errText(o); return; }
          let cred;
          try {
            const pk = o.data.publicKey;
            cred = await navigator.credentials.create({ publicKey: Object.assign({}, pk, { challenge: b64u.dec(pk.challenge), user: Object.assign({}, pk.user, { id: b64u.dec(pk.user.id) }), excludeCredentials: (pk.excludeCredentials || []).map(c => Object.assign({}, c, { id: b64u.dec(c.id) })) }) });
          } catch (e) { msg.textContent = 'The browser did not create the key (' + (e && e.name || 'cancelled') + ').'; return; }
          const x = await api('POST', '/api/v1/me/passkeys', { name: name.value, client_data_json: b64u.enc(cred.response.clientDataJSON), attestation_object: b64u.enc(cred.response.attestationObject) });
          if (x.status !== 201) { msg.textContent = errText(x); return; }
          close(); toast('Key added'); loadKeys();
        } }, 'Add'))));
      name.focus();
    }
    function removeKey(k) {
      const pw = h('input', { type: 'password', autocomplete: 'current-password', 'aria-label': 'Your password' }), msg = h('div', { class: 'err', role: 'alert' });
      const close = modal(h('div', {}, h('h2', {}, 'Remove ' + k.name), h('p', {}, 'Enter your password to confirm.'), pw, msg,
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('DELETE', '/api/v1/me/passkeys/' + k.id, { password: pw.value });
          if (x.status === 200) { close(); toast('Key removed'); loadKeys(); } else msg.textContent = x.status === 403 ? 'That password is not right.' : errText(x);
        } }, 'Remove'))));
      pw.focus();
    }

    // ----- password
    const cur = h('input', { type: 'password', autocomplete: 'current-password', id: 'pw-cur' }), nw = h('input', { type: 'password', autocomplete: 'new-password', id: 'pw-new' }), pmsg = h('div', { role: 'status', 'aria-live': 'polite' });
    const pwBox = h('form', { onsubmit: async e => {
      e.preventDefault();
      const r = await api('POST', '/api/v1/me/password', { current: cur.value, new: nw.value });
      pmsg.className = r.status === 200 ? 'muted' : 'err';
      pmsg.textContent = r.status === 200 ? 'Password changed. Your other sessions were signed out.' : r.status === 403 ? 'The current password is not right.' : errText(r);
      if (r.status === 200) { cur.value = ''; nw.value = ''; loadSessions(); }
    } }, h('label', { for: 'pw-cur' }, 'Current password'), cur, h('label', { for: 'pw-new' }, 'New password'), nw, pmsg, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Change password')));

    // ----- sessions
    const sessBox = h('div', { role: 'list' });
    async function loadSessions() {
      clear(sessBox);
      const r = await api('GET', '/api/v1/me/sessions');
      if (r.status !== 200) { sessBox.append(h('p', { class: 'err' }, errText(r))); return; }
      for (const s of r.data) {
        sessBox.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, (s.user_agent || 'Unknown device') + (s.current ? ' (this session)' : '')),
          h('span', { class: 'muted' }, (s.ip || '') + ' · last active ' + when(s.last_seen)),
          s.current ? null : h('button', { class: 'btn ghost', 'aria-label': 'Sign out ' + (s.user_agent || 'device') + ' ' + s.id, onclick: async () => { const x = await api('DELETE', '/api/v1/me/sessions/' + s.id); if (x.status === 200) loadSessions(); else toast(errText(x)); } }, 'Sign out')));
      }
    }
    const others = h('button', { class: 'btn', onclick: async () => { const x = await api('POST', '/api/v1/me/sessions/revoke-others'); if (x.status === 200) { toast(x.data.ended + ' session(s) signed out'); loadSessions(); } } }, 'Sign out everywhere else');

    // ----- API tokens
    const tokBox = h('div', { role: 'list' });
    async function loadTokens() {
      clear(tokBox);
      const r = await api('GET', '/api/v1/me/tokens');
      if (r.status !== 200) { tokBox.append(h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.length) tokBox.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'You have no API tokens.')));
      for (const t of r.data) {
        const state = t.revoked ? 'revoked' : t.expires && new Date(t.expires) < new Date() ? 'expired' : t.expires ? 'expires ' + when(t.expires) : 'does not expire';
        tokBox.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, t.name + (t.scopes && t.scopes.length ? ' (agent: ' + t.scopes.join(', ') + ')' : '')),
          h('span', { class: 'muted' }, state + ' · last used ' + when(t.last_used)),
          t.revoked ? null : h('button', { class: 'btn ghost', 'aria-label': 'Revoke token ' + t.name, onclick: async () => { const x = await api('DELETE', '/api/v1/me/tokens/' + t.id); if (x.status === 200) loadTokens(); else toast(errText(x)); } }, 'Revoke')));
      }
    }
    const tname = h('input', { type: 'text', 'aria-label': 'Token name', placeholder: 'Name, such as ci' }), tdays = h('select', { 'aria-label': 'Lifetime' }, [['30', '30 days'], ['90', '90 days'], ['365', '1 year'], ['0', 'Does not expire']].map(([v, l]) => h('option', { value: v }, l)));
    const tmsg = h('div', { class: 'err', role: 'alert' });
    const newTok = h('button', { class: 'btn', onclick: async () => {
      tmsg.textContent = '';
      const r = await api('POST', '/api/v1/me/tokens', { name: tname.value, expires_in_days: Number(tdays.value) });
      if (r.status !== 201) { tmsg.textContent = errText(r); return; }
      tname.value = ''; loadTokens();
      const close = modal(h('div', {}, h('h2', {}, 'Your new token'), h('p', {}, 'Copy it now. It is not shown again.'), h('pre', { 'aria-label': 'Token', style: 'user-select:all;white-space:pre-wrap;word-break:break-all' }, r.data.token),
        h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Done'))));
    } }, 'Create token');

    // ----- connected apps (OAuth)
    const appsBox = h('div', { role: 'list' });
    async function loadApps() {
      clear(appsBox);
      const r = await api('GET', '/api/v1/me/oauth/grants');
      if (r.status !== 200) { appsBox.append(h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.length) appsBox.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No applications are connected to your account.')));
      for (const g of r.data) appsBox.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'sum' }, h('strong', {}, g.client), ' ', h('span', { class: 'muted' }, 'can ' + g.scopes.join(', '))),
        h('span', { class: 'muted' }, 'last used ' + when(g.last_used)),
        h('button', { class: 'btn ghost', 'aria-label': 'Disconnect ' + g.client, onclick: async () => { const x = await api('DELETE', '/api/v1/me/oauth/grants/' + g.id); if (x.status === 200) loadApps(); else toast(errText(x)); } }, 'Disconnect')));
    }

    const panels = {
      Security: h('div', {},
        section('Two-factor authentication', mfaBox),
        section('Passkeys and security keys', h('p', { class: 'muted' }, 'A passkey or security key can finish signing in after your password, beside an authenticator app. Adding or removing one asks for your password again.'), keysBox),
        section('Password', pwBox)),
      Sessions: h('div', {}, section('Sessions', h('div', { class: 'chips' }, others), sessBox)),
      'API tokens': h('div', {},
        section('Connected applications', h('p', { class: 'muted' }, 'Applications such as AI assistants that you allowed to act for you, with the permissions you gave them. They never do more than you can.'), appsBox),
        section('API tokens', h('div', { class: 'chips' }, tname, tdays, newTok), tmsg, tokBox)),
    };
    Promise.all([loadMFA(), loadKeys(), loadSessions(), loadTokens(), loadApps()]);
    return panels;
  }

  // #/account is where a person is sent who must set up a second factor (the organization requires one) and where old links point:
  // they are sent on to Settings on its Security tab. Nothing behind it asks the server for anything, so a refusal cannot send the
  // person back here in a loop.
  function accountView() { A.S.settingsFrom = A.S.settingsFrom || '#/'; location.replace('#/settings/security'); }
  A.EXT.accountSections = buildAccount;

  // the screen an application sends a person to when it asks for access (OAuth authorization)
  async function consentView(query) {
    const app = clear(document.getElementById('app'));
    const params = Object.fromEntries(new URLSearchParams(query));
    const box = h('div', { class: 'login card', role: 'region', 'aria-label': 'Connect an application' });
    app.append(topbar(), h('div', { class: 'page' }, box));
    const r = await api('GET', '/api/v1/oauth/consent?' + query);
    if (r.status !== 200) { box.append(h('h1', {}, 'Cannot connect'), h('p', { class: 'err', role: 'alert' }, errText(r)), h('p', {}, h('a', { href: '#/' }, 'Back to Tasklane'))); return; }
    const decide = async approve => {
      const x = await api('POST', '/api/v1/oauth/authorize', Object.assign({}, params, { approve }));
      if (x.status === 200) { location.href = x.data.redirect; } else { toast(errText(x)); }
    };
    box.append(h('h1', {}, 'Connect ' + r.data.client), h('p', {}, h('strong', {}, r.data.client), ' wants to act for you in Tasklane. It will be able to:'),
      h('ul', {}, r.data.scopes.map(s => h('li', {}, s.description || s.name))),
      h('p', { class: 'muted' }, 'It never does more than you are allowed to do yourself, and you can disconnect it at any time under Account. After you decide you go back to ' + r.data.redirect_to + '.'),
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => decide(false) }, 'Deny'), h('button', { class: 'btn primary', onclick: () => decide(true) }, 'Allow')));
  }

  A.EXT.routes.push({ re: /^#\/oauth\/consent\?(.*)$/, run: m => consentView(m[1]) });
  A.EXT.routes.push({ re: /^#\/account\/?$/, run: () => accountView() });
})();
