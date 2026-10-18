// The account page: two-factor authentication, password, sessions and API tokens.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const when = iso => iso ? new Date(iso).toLocaleString() : 'never';

  async function accountView() {
    const app = clear(document.getElementById('app'));
    const body = h('div', { class: 'page' });
    app.append(topbar(), h('nav', { class: 'tabs', 'aria-label': 'Account' }, h('span', { class: 'tabs-title' }, 'Account and security')), body);
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
        h('p', {}, 'In your authenticator app add an account with this key, then enter the code it shows.'),
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

    body.append(
      section('Two-factor authentication', mfaBox),
      section('Password', pwBox),
      section('Sessions', h('div', { class: 'chips' }, others), sessBox),
      section('API tokens', h('div', { class: 'chips' }, tname, tdays, newTok), tmsg, tokBox));
    await Promise.all([loadMFA(), loadSessions(), loadTokens()]);
  }

  A.EXT.routes.push({ re: /^#\/account\/?$/, run: () => accountView() });
})();
