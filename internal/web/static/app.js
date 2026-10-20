(function () {
  'use strict';
  const L = window.TL;
  const $app = document.getElementById('app');
  const S = { me: null, projects: [], users: [], q: '', syntax: 'text', tql: '', view: null, settings: null, timers: [], nav: { keys: [], q: null }, vcache: new Map() };

  // ---------- helpers ----------
  function h(tag, props, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') e.className = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else if (k === 'value') e.value = v;
      else if (v === true) e.setAttribute(k, '');
      else e.setAttribute(k, v);
    }
    for (const c of kids.flat()) if (c !== null && c !== undefined && c !== false) e.append(c.nodeType ? c : document.createTextNode(String(c)));
    return e;
  }
  function clear(n) { while (n.firstChild) n.removeChild(n.firstChild); return n; }
  // The page says which client it is (X-Tasklane-Client); a server that no longer serves it answers 426 and the page asks for a reload.
  // A 503 "paused" means maintenance is under way: a banner says so, reads continue, and saves keep their drafts and retry when it ends.
  // WebAuthn carries binary values as base64url; the browser API wants buffers
  const b64u = {
    enc: buf => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
    dec: s => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), c => c.charCodeAt(0)),
  };
  const CLIENT_VERSION = 1;
  async function api(method, path, body, headers) {
    const r = await fetch(path, { method, credentials: 'same-origin', headers: Object.assign({ 'X-Requested-With': 'tasklane', 'Content-Type': 'application/json', 'X-Tasklane-Client': String(CLIENT_VERSION) }, headers || {}), body: body === undefined ? undefined : JSON.stringify(body) });
    let data = null; try { data = await r.json(); } catch (e) { /* empty body, 304 */ }
    if (r.status === 426 && data && data.code === 'client_too_old') showReload();
    else if (r.status === 503 && data && data.code === 'paused') showMaintenance();
    else if (r.status === 403 && data && data.code === 'second_factor_required') showSecondFactorRequired();
    else if (r.status === 403 && data && data.code === 'ip_not_allowed') banner('danger', 'This network is not allowed for your organization. Ask an administrator, or connect from an allowed network.');
    return { status: r.status, data, headers: r.headers };
  }
  function banner(kind, ...kids) {
    const b = document.getElementById('banner');
    if (!b) return;
    clear(b);
    if (kind) b.append(h('div', { class: 'banner ' + kind, role: 'status' }, ...kids));
  }
  const MAINT = { on: false, timer: 0 };
  function showMaintenance() {
    if (MAINT.on) return;
    MAINT.on = true;
    banner('warn', 'Maintenance in progress. You can keep reading; changes you make are kept as drafts and saved when it ends.');
    MAINT.timer = setInterval(async () => {
      let over = false;
      try { const r = await fetch('/api/v1/system/status', { credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane', 'X-Tasklane-Client': String(CLIENT_VERSION) } }); const d = r.ok ? await r.json() : null; over = !!d && (d.maintenance || []).length === 0; } catch (e) { /* the server may be restarting: keep waiting */ }
      if (over) { clearInterval(MAINT.timer); MAINT.on = false; banner(null); window.dispatchEvent(new Event('tl-resumed')); }
    }, 3000);
  }
  // the organization requires a second factor and this person has none: everything but the pages that set one up is refused, so
  // take them there and say why
  function showSecondFactorRequired() {
    banner('warn', 'Your organization requires a second factor to sign in. Set up an authenticator app or a passkey to continue.');
    if (location.hash !== '#/account') location.hash = '#/account';
  }
  function showReload() {
    banner('warn', 'A newer version of Tasklane is available. ', h('button', { class: 'btn', onclick: () => location.reload() }, 'Reload'), ' Your drafts are kept.');
  }
  function toast(msg, action) {
    const t = h('div', { class: 'toast' }, h('span', {}, msg));
    if (action) t.append(h('button', { class: 'btn', onclick: () => { action.fn(); t.remove(); } }, action.label));
    t.append(h('button', { class: 'btn ghost', 'aria-label': 'Dismiss', onclick: () => t.remove() }, '×'));
    document.getElementById('toasts').append(t);
    setTimeout(() => t.remove(), 12000);
  }
  // Undo instead of confirm (SPEC 4.6): shown for 5 s, held while the pointer is on it or it has focus; Undo asks the server, which
  // reverts only if nobody changed the issue since and says so otherwise. Toasts stack; the region says each one once.
  const WANT_UNDO = { 'Tasklane-Want-Undo': '1' };
  function undoToast(msg, r, after) {
    const token = r && r.headers && r.headers.get('Tasklane-Undo');
    if (!token) { toast(msg); return; }
    const t = h('div', { class: 'toast' }, h('span', {}, msg));
    let left = 5000, started = Date.now(), timer = 0, held = 0;
    const gone = () => { clearTimeout(timer); t.remove(); };
    const run = () => { started = Date.now(); timer = setTimeout(gone, left); };
    const hold = () => { if (held++ === 0) { clearTimeout(timer); left -= Date.now() - started; } };
    const release = () => { if (--held === 0) run(); };
    t.addEventListener('mouseenter', hold); t.addEventListener('mouseleave', release);
    t.addEventListener('focusin', hold); t.addEventListener('focusout', e => { if (!t.contains(e.relatedTarget)) release(); });
    const undo = h('button', { class: 'btn', onclick: async () => {
      undo.disabled = true;
      const x = await api('POST', '/api/v1/undo/' + encodeURIComponent(token));
      gone();
      if (x.status === 200) { toast('Undone.'); if (after) after(x.data); }
      else if (x.status === 409) toast((x.data && x.data.error) || 'It changed since, so it was not undone.');
      else if (x.status === 404) toast('It is too late to undo that.');
      else toast((x.data && x.data.error) || 'Could not undo.');
    } }, 'Undo');
    t.append(undo, h('button', { class: 'btn ghost', 'aria-label': 'Dismiss', onclick: gone }, '×'));
    document.getElementById('toasts').append(t);
    run();
  }
  const initials = n => (n || '?').split(/\s+/).map(w => w[0]).slice(0, 2).join('').toUpperCase();
  const PRES = ['#F27AA4', '#F4A261', '#E9C46A', '#7BD389', '#5FD0C5', '#7AAEF7', '#B39DF5', '#E58FE0'];
  const presColor = id => PRES[[...id].reduce((a, c) => a + c.charCodeAt(0), 0) % 8];
  const avatar = (name, ring) => h('span', { class: 'av', title: name, style: ring ? 'border-color:' + ring : '' }, initials(name));
  const PRI = { highest: '⇈', high: '↑', medium: '=', low: '↓', lowest: '⇊' };
  const catLabel = { todo: 'To Do', in_progress: 'In Progress', done: 'Done' };
  const lozenge = (name, cat) => h('span', { class: 'loz ' + cat }, name);
  function paragraphs(text) {
    const box = h('div', { class: 'body' });
    for (const p of text.split('\n\n')) {
      const para = h('p');
      for (const part of L.inline(p)) para.append(part.href ? h('a', { href: part.href, rel: 'noopener noreferrer', target: '_blank' }, part.text) : part.text);
      box.append(para);
    }
    return box;
  }
  function stopTimers() { S.timers.forEach(clearInterval); S.timers = []; if (S.cleanup) S.cleanup(); }
  function theme(t) { document.documentElement.dataset.theme = t; try { localStorage.setItem('tl-theme', t); } catch (e) { /* private mode */ } }
  try { theme(localStorage.getItem('tl-theme') || 'dark'); } catch (e) { /* default dark */ }

  // ---------- realtime: presence and change events; best effort, pages work without it ----------
  const RT = { ws: null, up: false, subs: new Map(), backoff: 1000, timer: 0, hint: null, off: false };
  async function rtConnect() {
    clearTimeout(RT.timer);
    if (!S.me || RT.ws || RT.off) return;
    let tk;
    try {
      const r = await api('POST', '/api/v1/ws/ticket');
      if (r.status === 404) { RT.off = true; return; } // the realtime layer is switched off on this server: pages work without it (presence by HTTP, saves by REST, conflicts by version)
      if (r.status !== 200) throw new Error('no ticket'); tk = r.data.ticket;
    } catch (e) { return rtRetry(); }
    const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/v1/ws?ticket=' + encodeURIComponent(tk));
    RT.ws = ws;
    ws.onopen = () => { RT.up = true; RT.backoff = 1000; for (const k of RT.subs.keys()) ws.send(JSON.stringify({ op: 'sub', channel: 'issue:' + k })); };
    ws.onmessage = ev => { let m; try { m = JSON.parse(ev.data); } catch (e) { return; } if (m.type === 'reconnect') { RT.hint = Number(m.after_ms) || 0; RT.backoff = 1000; return; } /* the server is stopping: come back when it says, spread out */ const k = m.issue || (m.event && m.event.issue) || (m.channel || '').replace('issue:', ''); const h = RT.subs.get(k); if (h) h(m); };
    ws.onclose = () => { RT.up = false; RT.ws = null; rtRetry(); };
    ws.onerror = () => { try { ws.close(); } catch (e) { /* already closed */ } };
  }
  function rtRetry() { if (!S.me || RT.off) return; const wait = RT.hint !== null ? RT.hint : RT.backoff * (0.5 + Math.random()); RT.hint = null; RT.timer = setTimeout(rtConnect, wait); RT.backoff = Math.min(RT.backoff * 2, 30000); } // jittered, so a restart does not bring every tab back in the same second
  function rtSub(key, handler) { RT.subs.set(key, handler); if (RT.up) RT.ws.send(JSON.stringify({ op: 'sub', channel: 'issue:' + key })); else rtConnect(); }
  function rtUnsub(key) { RT.subs.delete(key); if (RT.up) RT.ws.send(JSON.stringify({ op: 'unsub', channel: 'issue:' + key })); }
  function rtSend(m) { if (RT.up) { RT.ws.send(JSON.stringify(m)); return true; } return false; }

  // ---------- shell ----------
  // the less used places, folded into one menu so the bar fits at any width (a disclosure, so it works with the keyboard and screen readers)
  // ----- the person's own settings (SPEC 4.6): held by the server (one row, versioned), applied here and cached for theme.js's first paint
  const PREF_DEFAULTS = { list_view: 'short', page_width: 'wide', snippet_chars: 500, theme: '', density: 'default', columns: ['type', 'key', 'summary', 'priority', 'assignee', 'status', 'updated'], sort: { field: '', dir: 'asc' }, font_scale: 100, reduced_motion: 'system', high_contrast: false, shortcuts: true, hints_seen: [] };
  const P = { version: 0, queue: Promise.resolve(), listeners: [] };
  S.settings = Object.assign({}, PREF_DEFAULTS);
  function applyWidth() { document.body.dataset.width = S.settings.page_width; }
  function applyPrefs() {
    const r = document.documentElement, p = S.settings;
    if (p.theme === 'light' || p.theme === 'dark') theme(p.theme);
    r.dataset.density = p.density;
    if (p.font_scale === 100) delete r.dataset.fs; else r.dataset.fs = String(p.font_scale);
    if (p.high_contrast) r.dataset.hc = ''; else delete r.dataset.hc;
    r.dataset.motion = p.reduced_motion;
    applyWidth();
    try { localStorage.setItem('tl-ui', JSON.stringify(p)); } catch (e) { /* private mode: the server still has them */ }
    for (const fn of P.listeners) fn(p);
  }
  function loadPrefs(doc) { S.settings = Object.assign({}, PREF_DEFAULTS, doc ? doc.prefs : {}); P.version = doc ? doc.version : 0; applyPrefs(); }
  // the next person on this browser starts from the defaults, not the last person's settings (PREF-03)
  function forgetPrefs() { try { localStorage.removeItem('tl-ui'); } catch (e) { /* nothing stored */ } S.settings = Object.assign({}, PREF_DEFAULTS); P.version = 0; const r = document.documentElement; for (const k of ['density', 'fs', 'hc', 'motion']) delete r.dataset[k]; }
  // a change applies at once; saves go one at a time with If-Match, and when another tab saved first the 412 carries its settings:
  // this change goes on top of them and is saved again, so neither tab's change is lost (PREF-02). Resolves true when it was saved.
  function saveSettings(ch) {
    const before = Object.assign({}, S.settings);
    Object.assign(S.settings, ch); applyPrefs();
    const run = async () => {
      for (let i = 0; i < 3; i++) {
        const r = await api('PUT', '/api/v1/me/ui-prefs', ch, { 'If-Match': '"' + P.version + '"' });
        if (r.status === 200) { P.version = r.data.version; return true; }
        if (r.status === 412 && r.data && r.data.detail) { P.version = r.data.detail.version; S.settings = Object.assign({}, PREF_DEFAULTS, r.data.detail.prefs, ch); applyPrefs(); continue; }
        for (const k of Object.keys(ch)) S.settings[k] = before[k]; // only this change is undone; a later one already applied stays
        applyPrefs();
        toast((r.data && r.data.error) || 'Could not save the settings'); return false;
      }
      return false;
    };
    const done = P.queue.then(run, run);
    P.queue = done.catch(() => false);
    return done;
  }
  const SVGNS = 'http://www.w3.org/2000/svg';
  // an icon is a list of [tag, attributes]; built with DOM calls, never from a string
  const svg = shapes => {
    const e = document.createElementNS(SVGNS, 'svg');
    for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', width: 18, height: 18, 'aria-hidden': 'true', fill: 'none', stroke: 'currentColor', 'stroke-width': 1.6 })) e.setAttribute(k, v);
    for (const [tag, attrs] of shapes) { const c = document.createElementNS(SVGNS, tag); for (const [k, v] of Object.entries(attrs)) c.setAttribute(k, v); e.append(c); }
    return e;
  };
  const ICON = {
    wide: [['rect', { x: 2, y: 5, width: 20, height: 14, rx: 1.5 }], ['path', { d: 'M6 12h12M6 12l2.5-2.5M6 12l2.5 2.5M18 12l-2.5-2.5M18 12l-2.5 2.5' }]], // the page spread over the whole window
    narrow: [['rect', { x: 7, y: 3, width: 10, height: 18, rx: 1 }], ['path', { d: 'M2.5 12h3M2.5 12l1.5-1.5M2.5 12l1.5 1.5M21.5 12h-3M21.5 12l-1.5-1.5M21.5 12l-1.5 1.5' }]], // a page like the printed one
    short: [['path', { d: 'M4 7h16M4 12h16M4 17h16' }]],
    long: [['path', { d: 'M4 5h16M4 8h12M4 13h16M4 16h12M4 21h8' }]]
  };
  // one group of toggle buttons: wide or narrow page, and (on the list) short or long rows
  function viewControls(withRows, onRows) {
    const btn = (icon, label, on, fn) => h('button', { class: 'btn' + (on ? '' : ' ghost'), 'aria-pressed': String(on), title: label, 'aria-label': label, onclick: fn }, svg(ICON[icon]));
    const box = h('span', { class: 'viewctl', role: 'group', 'aria-label': 'View' });
    const draw = () => {
      clear(box);
      box.append(btn('wide', 'Wide view (the whole window)', S.settings.page_width === 'wide', async () => { await saveSettings({ page_width: 'wide' }); draw(); }),
        btn('narrow', 'Narrow view (like a printed page)', S.settings.page_width === 'narrow', async () => { await saveSettings({ page_width: 'narrow' }); draw(); }));
      if (withRows) {
        box.append(h('span', { class: 'sep' }),
          btn('short', 'Short view: title, status, assignee', S.settings.list_view === 'short', async () => { if (await saveSettings({ list_view: 'short' })) { draw(); onRows(); } }),
          btn('long', 'Long view: with the start of the description', S.settings.list_view === 'long', async () => { if (await saveSettings({ list_view: 'long' })) { draw(); onRows(); } }),
          h('span', { class: 'sep' }));
        const den = h('span', { class: 'seg', role: 'group', 'aria-label': 'Density' });
        for (const [v, label] of [['compact', 'Compact'], ['default', 'Default'], ['comfortable', 'Comfortable']])
          den.append(h('button', { class: 'btn' + (S.settings.density === v ? '' : ' ghost'), 'aria-pressed': String(S.settings.density === v), title: label + ' rows', onclick: async () => { saveSettings({ density: v }); draw(); } }, label));
        box.append(den);
      }
    };
    draw();
    return box;
  }
  function settingsDialog() { if (EXT.settings) EXT.settings(); }

  function moreMenu() {
    const d = h('details', { class: 'topmore' });
    const go = (label, hash) => h('a', { href: hash, class: 'topmore-item', onclick: () => d.removeAttribute('open') }, label);
    const first = S.projects[0] ? S.projects[0].key : '';
    d.append(h('summary', { class: 'btn ghost' }, 'More'),
      h('div', { class: 'topmore-list' }, go('Dashboards', '#/dashboards'), go('Releases', '#/releases'), go('Approvals', '#/approvals'), go('Queues', '#/queues'), go('Import', '#/import'), go('Trash', '#/trash/' + first)));
    d.addEventListener('keydown', e => { if (e.key === 'Escape') { d.removeAttribute('open'); d.querySelector('summary').focus(); } });
    return d;
  }

  function topbar() {
    // the search box speaks plain text (Alan AND (Arkady OR Dana)) by default, or TQL; the URL keeps which, so a link reopens the same search
    const hint = { text: 'Search…  e.g. Alan AND (Arkady OR Dana) -draft', tql: 'TQL…  e.g. project = ABC AND status = "To Do"' };
    const q = h('input', { type: 'search', placeholder: hint[S.syntax], class: 'grow', 'aria-label': S.syntax === 'tql' ? 'Search in TQL' : 'Search', value: S.q });
    const search = () => {
      S.q = q.value;
      const target = '#/?q=' + encodeURIComponent(q.value) + '&syntax=' + S.syntax;
      if (location.hash === target) route(); // the same search again is a refresh: no hashchange fires for an identical hash
      else location.hash = target;
    };
    q.addEventListener('keydown', e => { if (e.key === 'Enter') search(); });
    const mode = (m, label, title) => h('button', { class: 'btn' + (S.syntax === m ? '' : ' ghost'), 'aria-pressed': S.syntax === m ? 'true' : 'false', title, onclick: () => {
      if (S.syntax === m) return;
      // switching runs nothing: text to TQL shows the search as the TQL it ran as, to edit further; TQL has no plain-text form
      if (m === 'tql') { if (q.value.trim() === S.q.trim() && S.tql) q.value = S.tql; } else q.value = '';
      S.syntax = m;
      q.placeholder = hint[m]; q.setAttribute('aria-label', m === 'tql' ? 'Search in TQL' : 'Search');
      for (const b of seg.children) { const on = b.dataset.m === m; b.className = 'btn' + (on ? '' : ' ghost'); b.setAttribute('aria-pressed', on ? 'true' : 'false'); }
      q.focus();
    }, 'data-m': m }, label);
    const seg = h('span', { class: 'syntax', role: 'group', 'aria-label': 'Search syntax' });
    seg.append(mode('text', 'Text', 'Words, "phrases", AND / OR / NOT, -word; matches text, keys and people'), mode('tql', 'TQL', 'Tasklane Query Language: field = value …'));
    return h('div', { class: 'bar' },
      h('span', { class: 'brand', tabindex: 0, role: 'link', onclick: () => { location.hash = '#/'; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/'; } }, 'Tasklane'),
      h('span', { class: 'search grow' }, q, seg),
      plusMenu(),
      h('button', { class: 'btn ghost', id: 'bell', 'aria-label': 'Inbox', onclick: () => { location.hash = '#/inbox/unread'; } }, 'Inbox', h('span', { id: 'bell-n', class: 'badge', hidden: true })),
      h('button', { class: 'btn ghost', onclick: () => { location.hash = '#/boards/' + (S.projects[0] ? S.projects[0].key : ''); } }, 'Boards'),
      moreMenu(),
      S.me && S.me.admin ? h('button', { class: 'btn ghost', onclick: () => { location.hash = '#/admin/workflows'; } }, 'Admin') : null,
      h('button', { class: 'btn ghost', id: 'settings-btn', 'aria-label': 'Settings', 'aria-haspopup': 'dialog', title: 'Settings', onclick: settingsDialog }, '⚙'),
      h('button', { class: 'btn ghost', 'aria-label': 'Toggle theme', onclick: () => saveSettings({ theme: document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark' }) }, '◐'),
      S.me ? h('button', { class: 'btn ghost', 'aria-label': 'Account and security', onclick: () => { location.hash = '#/account'; } }, S.me.label) : null,
      h('button', { class: 'btn ghost', onclick: async () => { await api('POST', '/api/v1/logout'); S.me = null; forgetPrefs(); route(); } }, 'Sign out'));
  }

  function loginView() {
    const email = h('input', { type: 'text', id: 'email', autocomplete: 'username', inputmode: 'email', autocapitalize: 'none', spellcheck: 'false' }), pw = h('input', { type: 'password', id: 'pw', autocomplete: 'current-password' });
    const msg = h('div', { class: 'err', role: 'alert' });
    // the second step: a passkey or security key, and/or a code from the authenticator app or a recovery code
    const codeStep = (mfaToken, totp, key) => {
      const code = h('input', { type: 'text', id: 'code', autocomplete: 'one-time-code', inputmode: 'numeric', 'aria-label': 'Authentication code' });
      const err = h('div', { class: 'err', role: 'alert' });
      const useKey = async () => {
        err.textContent = '';
        try {
          const pk = key.publicKey;
          const cred = await navigator.credentials.get({ publicKey: Object.assign({}, pk, { challenge: b64u.dec(pk.challenge), allowCredentials: (pk.allowCredentials || []).map(c => Object.assign({}, c, { id: b64u.dec(c.id) })) }) });
          const r = await api('POST', '/api/v1/login/passkey', { mfa_token: mfaToken, id: b64u.enc(cred.rawId), client_data_json: b64u.enc(cred.response.clientDataJSON), authenticator_data: b64u.enc(cred.response.authenticatorData), signature: b64u.enc(cred.response.signature) });
          if (r.status === 200) { await boot(); } else err.textContent = 'That key could not sign you in. Start again from your password.';
        } catch (e) { err.textContent = 'The key was not used. You can try again.'; }
      };
      const kids = [h('h1', {}, 'Tasklane')];
      if (key) kids.push(h('p', {}, 'Use your passkey or security key to finish signing in.'), h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'button', onclick: useKey }, 'Use passkey or security key')));
      if (totp) kids.push(h('p', {}, key ? 'Or enter the code from your authenticator app, or a recovery code.' : 'Enter the code from your authenticator app, or a recovery code.'), h('label', { for: 'code' }, 'Authentication code'), code,
        h('div', { class: 'actions' }, h('button', { class: 'btn' + (key ? '' : ' primary'), type: 'submit' }, 'Verify')));
      kids.push(err);
      clear($app).append(h('form', { class: 'login card', onsubmit: async e => {
        e.preventDefault();
        if (!totp) return;
        const r = await api('POST', '/api/v1/login/mfa', { mfa_token: mfaToken, code: code.value });
        if (r.status === 200) { await boot(); } else { err.textContent = (r.data && r.data.error) || 'That code is not right'; code.value = ''; code.focus(); }
      } }, ...kids));
      if (totp && !key) code.focus();
    };
    const go = async e => {
      e.preventDefault();
      const r = await api('POST', '/api/v1/login', { email: email.value, password: pw.value });
      if (r.status === 200 && r.data.mfa_required) { codeStep(r.data.mfa_token, r.data.totp, r.data.passkey); return; }
      if (r.status === 200) { await boot(); } else msg.textContent = r.status === 429 ? 'Too many failed attempts. Try again in a few minutes.' : (r.data && r.data.error) || 'Sign in failed';
    };
    const sso = h('div', { class: 'sso' });
    if (new URLSearchParams(location.search).get('sso') === 'denied') msg.textContent = 'Single sign-on was not allowed for this account.';
    clear($app).append(h('form', { class: 'login card', onsubmit: go },
      h('h1', {}, 'Tasklane'), h('label', { for: 'email' }, 'Email or username'), email, h('label', { for: 'pw' }, 'Password'), pw, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Sign in')), sso));
    email.focus();
    api('GET', '/api/v1/auth/sso/providers').then(r => {
      if (r.status !== 200) return;
      for (const p of r.data) sso.append(h('a', { class: 'btn', style: 'display:block;text-align:center;margin-top:8px', href: '/api/v1/auth/sso/' + encodeURIComponent(p.name) + '/start?redirect=' + encodeURIComponent('/' + (location.hash || '')) }, 'Sign in with ' + p.label));
    });
  }

  // ---------- issue list ----------
  async function listView(q) {
    stopTimers();
    S.q = q || '';
    const out = h('div', { class: 'card', role: 'list' }), err = h('div', { class: 'tqlerr', role: 'alert' }), meta = h('div', { class: 'muted' });
    const pager = h('div', { class: 'actions', style: 'margin-top:8px' });
    let at = 0; // the page of results on the screen
    const controls = viewControls(true, () => load(at)); // wide or narrow, short or long rows
    // the page's checkbox: all or none of the rows on this page (a bulk action takes one page at most)
    const all = h('input', { type: 'checkbox', class: 'rowsel', 'aria-label': 'Select all on this page', hidden: true });
    let cascading = false;
    all.addEventListener('change', () => {
      const want = all.checked;
      cascading = true; // the rows are changed one by one: the page checkbox keeps its new state until they all are
      for (const b of out.querySelectorAll('input.rowsel')) if (b.checked !== want) { b.checked = want; b.dispatchEvent(new Event('change')); }
      cascading = false;
      syncAll();
    });
    const syncAll = () => { if (cascading) return; const n = out.querySelectorAll('input.rowsel').length; all.checked = n > 0 && S.sel.size === n; all.indeterminate = S.sel.size > 0 && S.sel.size < n; };
    clear($app).append(topbar(), h('div', { class: 'page fluid' }, h('div', { class: 'listbar' }, h('span', { class: 'listsel' }, all, meta), controls), err, out, pager));
    const size = 100;
    const load = async n => {
      at = n;
      const long = S.settings.list_view === 'long';
      const r = await api('GET', '/api/v1/issues?limit=' + size + '&offset=' + n * size + '&q=' + encodeURIComponent(S.q) + '&syntax=' + S.syntax + (long ? '&snippet=' + S.settings.snippet_chars : ''));
      if (r.status === 400) { err.textContent = r.data.error + (r.data.detail && r.data.detail.pos != null ? ' (at position ' + r.data.detail.pos + ')' : ''); return; }
      if (r.status !== 200) { err.textContent = 'Could not load issues'; return; }
      S.tql = r.data.query || '';
      S.nav = { keys: r.data.issues.map(i => i.key), q: S.q }; // the order the person is reading in: prev/next in a ticket follow it
      clear(out); clear(pager);
      const pages = Math.max(1, Math.ceil(r.data.total / size));
      const from = n * size + 1, to = n * size + r.data.issues.length;
      meta.textContent = (r.data.total_capped ? r.data.total + '+' : r.data.total) + ' issues' + (pages > 1 && r.data.issues.length ? ' · ' + from + '–' + to : '');
      if (r.data.warning) meta.append(' · ', h('span', { class: 'note', role: 'status' }, r.data.warning)); // the index is behind, or a fallback answered
      if (!r.data.issues.length) out.append(h('div', { class: 'row' }, h('span', { class: 'muted' }, 'No issues match.')));
      S.sel = new Set(); S.cur = -1; S.lastPick = -1; // a new page starts with nothing selected and no current row (LIST-05)
      if (EXT.onSelect) EXT.onSelect();
      all.hidden = !r.data.issues.length; all.checked = false; all.indeterminate = false;
      for (const i of r.data.issues) {
        const open = () => { location.hash = '#/issue/' + i.key; };
        // the checkbox selects without opening (x does the same on the current row)
        const pick = h('input', { type: 'checkbox', class: 'rowsel', 'aria-label': 'Select ' + i.key, onclick: e => {
          e.stopPropagation(); // keys go on to the shortcuts; the row opens only on its own Enter
          const boxes = [...out.querySelectorAll('input.rowsel')], at = boxes.indexOf(pick);
          if (e.shiftKey && S.lastPick >= 0 && S.lastPick !== at) { // shift-click: everything between the last click and this one takes this one's state
            const [a, b] = [Math.min(S.lastPick, at), Math.max(S.lastPick, at)];
            for (let k = a; k <= b; k++) if (boxes[k].checked !== pick.checked) { boxes[k].checked = pick.checked; boxes[k].dispatchEvent(new Event('change')); }
          }
          S.lastPick = at;
        } });
        pick.addEventListener('change', () => { if (pick.checked) S.sel.add(i.key); else S.sel.delete(i.key); syncAll(); if (EXT.onSelect) EXT.onSelect(); });
        out.append(h('div', { class: 'row' + (long ? ' long' : ''), role: 'listitem', tabindex: 0, 'data-key': i.key, onclick: open, onkeydown: e => { if (e.key === 'Enter' && e.target === e.currentTarget) open(); } },
          pick, h('span', { class: 'tile ' + i.type, title: i.type }), h('span', { class: 'key' }, i.key), h('span', { class: 'sum' }, i.summary),
          h('span', { class: 'pri ' + i.priority, title: i.priority }, PRI[i.priority] || ''), i.assignee ? avatar(i.assignee) : h('span', { class: 'av' }, '–'),
          lozenge(i.status, i.status_category), h('span', { class: 'muted' }, L.relTime(i.updated)),
          long ? h('div', { class: 'snip' }, i.snippet || h('span', { class: 'muted' }, 'No description.')) : null,
          h('span', { class: 'hover-acts', 'data-for': i.key })));
      }
      if (pages < 2) return;
      // numbered pages around the current one, with the first and last always reachable
      const go = k => async () => { await load(k); window.scrollTo(0, 0); };
      pager.append(h('button', { class: 'btn ghost', disabled: n === 0, onclick: go(n - 1) }, '‹ Prev'));
      let gap = false;
      for (let k = 0; k < pages; k++) {
        if (k !== 0 && k !== pages - 1 && Math.abs(k - n) > 2) { if (!gap) pager.append(h('span', { class: 'muted' }, '…')); gap = true; continue; }
        gap = false;
        pager.append(h('button', { class: k === n ? 'btn' : 'btn ghost', 'aria-current': k === n ? 'page' : null, onclick: go(k) }, String(k + 1)));
      }
      pager.append(h('button', { class: 'btn ghost', disabled: n >= pages - 1, onclick: go(n + 1) }, 'Next ›'));
    };
    await load(0);
  }


  // ----- custom fields: one control per field type; get() returns the JSON value to send (null clears, undefined means untouched/empty)
  function fieldControl(f, value, disabled) {
    const lab = f.name + (f.unit ? ' (' + f.unit + ')' : '');
    const base = { 'aria-label': lab, disabled: !!disabled, title: f.help_text || '' };
    const opts = (f.options || []).filter(o => !o.archived);
    const asList = v => Array.isArray(v) ? v : (v == null || v === '' ? [] : [v]);
    let el, get;
    switch (f.type) {
      case 'priority': el = h('select', base, Object.keys(PRI).map(p => h('option', { value: p }, PRI[p] + ' ' + p))); el.value = value || 'medium'; get = () => el.value; break;
      case 'textarea': el = h('textarea', Object.assign({ rows: 3 }, base)); el.value = value == null ? '' : value; get = () => el.value || null; break;
      case 'number': case 'float': case 'integer': case 'rating':
        el = h('input', Object.assign({ type: 'number', step: f.type === 'float' || f.type === 'number' ? 'any' : '1', style: 'width:120px' }, base, f.type === 'rating' ? { min: 0, max: 5 } : {}));
        el.value = value == null ? '' : value; get = () => el.value === '' ? null : Number(el.value); break;
      case 'duration': el = h('input', Object.assign({ type: 'text', placeholder: '2h 30m', style: 'width:120px' }, base)); el.value = value == null ? '' : (typeof value === 'number' ? L.fmtDuration ? L.fmtDuration(value) : value + 's' : value); get = () => el.value.trim() || null; break;
      case 'date': el = h('input', Object.assign({ type: 'date' }, base)); el.value = value || ''; get = () => el.value || null; break;
      case 'datetime': el = h('input', Object.assign({ type: 'datetime-local' }, base)); el.value = value ? String(value).slice(0, 16) : '';
        get = () => el.value ? new Date(el.value).toISOString().replace('.000', '') : null; break;
      case 'select': case 'radio':
        el = h('select', base, h('option', { value: '' }, '—'), opts.map(o => h('option', { value: o.value }, o.value))); el.value = value || ''; get = () => el.value || null; break;
      case 'multi_select': case 'checkbox_group': {
        const have = new Set(asList(value));
        el = h('select', Object.assign({ multiple: true, size: Math.min(5, Math.max(2, opts.length)) }, base), opts.map(o => h('option', { value: o.value, selected: have.has(o.value) }, o.value)));
        get = () => { const r = [...el.selectedOptions].map(o => o.value); return r.length ? r : null; }; break; }
      case 'user': case 'multi_user': {
        const have = new Set(asList(value).map(x => x && x.id ? x.id : x)), multi = f.type === 'multi_user';
        el = h('select', Object.assign(multi ? { multiple: true, size: 4 } : {}, base), multi ? null : h('option', { value: '' }, '—'), S.users.map(u => h('option', { value: u.id, selected: have.has(u.id) }, u.name)));
        if (!multi) el.value = [...have][0] || '';
        get = () => { const r = multi ? [...el.selectedOptions].map(o => o.value) : (el.value ? [el.value] : []); return r.length ? (multi ? r : r[0]) : null; }; break; }
      case 'labels': el = h('input', Object.assign({ type: 'text', placeholder: 'labels' }, base)); el.value = asList(value).join(' '); get = () => { const r = el.value.split(/[\s,]+/).filter(Boolean); return r.length ? r : null; }; break;
      default: el = h('input', Object.assign({ type: f.type === 'url' ? 'url' : 'text' }, base)); el.value = value == null ? '' : value; get = () => el.value.trim() || null;
    }
    return { el, get, name: lab };
  }
  // behavior rules, evaluated on the client as values change (the server enforces the same rules)
  function ruleHolds(r, vals, role) {
    if (r.when_role && r.when_role !== role) return false;
    if (!r.when_field) return true;
    const v = vals[r.when_field], empty = v == null || v === '' || (Array.isArray(v) && !v.length);
    const list = x => Array.isArray(x) ? x.map(String) : (x == null ? [] : [String(x)]);
    switch (r.when_op) {
      case 'empty': return empty;
      case 'notempty': return !empty;
      case 'eq': return !empty && (Array.isArray(v) ? list(v).includes(String(r.when_value)) : String(v) === String(r.when_value));
      case 'ne': return empty || (Array.isArray(v) ? !list(v).includes(String(r.when_value)) : String(v) !== String(r.when_value));
      case 'in': return !empty && list(v).some(x => list(r.when_value).includes(x));
    }
    return false;
  }
  function ruleState(rules, f, vals, role) {
    const st = { hidden: !!f.hidden, required: !!f.required };
    for (const r of rules || []) {
      if (r.target !== f.key || !r.enabled || !ruleHolds(r, vals, role)) continue;
      if (r.action === 'hide') st.hidden = true; else if (r.action === 'show') st.hidden = false; else if (r.action === 'require') st.required = true;
    }
    return st;
  }
  function fieldRow(f, ctl) {
    const star = h('span', { class: 'cf-name' }, f.name + (f.required ? ' *' : ''));
    ctl.star = star; ctl.base = f.name;
    return h('label', { class: 'cf-row' }, star, ctl.el, f.help_text ? h('span', { class: 'muted' }, f.help_text) : null);
  }

  // The "+" menu (as on GitHub): everything a person can create, in one place. An item is shown only to people the server would
  // let do it (the server stays the check); "c" opens New issue from anywhere that is not a text field.
  function plusMenu() {
    const me = S.me || {}, staff = !!(me.admin || me.org_admin);
    const wrap = h('div', { class: 'topmore topplus' });
    const btn = h('button', { class: 'btn primary', 'aria-label': 'Create new', 'aria-haspopup': 'menu', 'aria-expanded': 'false', title: 'Create new (c for a new issue)' }, '+ ▾');
    const list = h('div', { class: 'topmore-list', role: 'menu', 'aria-label': 'Create new', hidden: true });
    const shut = (back) => { list.hidden = true; btn.setAttribute('aria-expanded', 'false'); if (back) btn.focus(); };
    const item = (label, fn) => h('button', { class: 'topmore-item', role: 'menuitem', onclick: () => { shut(false); fn(); } }, label);
    list.append(item('New issue', createDialog), staff ? item('New project', projectDialog) : null, item('New dashboard', dashboardDialog),
      staff ? item('New user', userDialog) : null, me.admin ? item('New organization', organizationDialog) : null);
    btn.addEventListener('click', () => {
      const open = list.hidden; list.hidden = !open; btn.setAttribute('aria-expanded', String(open));
      if (open) list.querySelector('[role=menuitem]').focus();
    });
    wrap.addEventListener('keydown', e => {
      if (e.key === 'Escape' && !list.hidden) { e.preventDefault(); shut(true); return; }
      if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && !list.hidden) {
        e.preventDefault();
        const its = [...list.querySelectorAll('[role=menuitem]')], at = its.indexOf(document.activeElement);
        its[(at + (e.key === 'ArrowDown' ? 1 : its.length - 1)) % its.length].focus();
      }
    });
    wrap.addEventListener('focusout', e => { if (!wrap.contains(e.relatedTarget)) shut(false); });
    wrap.append(btn, list);
    return wrap;
  }
  // a small dialog: a title, labelled fields, an error line, Cancel and a submit that returns the response; done runs on success
  function formDialog(title, fields, submitLabel, submit, done) {
    const msg = h('div', { class: 'err', role: 'alert' });
    const go = h('button', { class: 'btn primary', onclick: async () => {
      msg.textContent = ''; go.disabled = true;
      try { const r = await submit(); if (r.status === 201 || r.status === 200) { close(); done(r.data); } else msg.textContent = (r.data && r.data.error) || 'Failed'; } finally { go.disabled = false; }
    } }, submitLabel);
    const close = modal(h('div', {}, h('h2', {}, title), h('div', { class: 'form-stack' }, fields), msg, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), go)));
    fields.forEach(f => f.addEventListener && f.addEventListener('keydown', ev => { if (ev.key === 'Enter' && ev.target.tagName === 'INPUT' && ev.target.type !== 'checkbox') go.click(); }));
    return close;
  }
  const field = (label, ctl) => h('label', { class: 'cf-row' }, h('span', { class: 'cf-name' }, label), ctl);
  async function reloadProjects() { const b = await api('GET', '/api/v1/bootstrap'); if (b.status === 200) S.projects = b.data.projects; }

  function projectDialog() {
    const key = h('input', { type: 'text', 'aria-label': 'Project key', placeholder: 'ABC', maxlength: 10, autocapitalize: 'characters', style: 'text-transform:uppercase' });
    const name = h('input', { type: 'text', 'aria-label': 'Project name', placeholder: 'My project', maxlength: 100 });
    formDialog('New project', [field('Key (2 to 10 letters and digits; the prefix of its issues, such as ABC-1)', key), field('Name', name)], 'Create project',
      () => api('POST', '/api/v1/projects', { key: key.value, name: name.value }),
      async p => { await reloadProjects(); toast('Project ' + p.key + ' created'); location.hash = '#/?q=' + encodeURIComponent('project = ' + p.key) + '&syntax=tql'; route(); });
  }
  function dashboardDialog() {
    const name = h('input', { type: 'text', 'aria-label': 'Dashboard name', placeholder: 'Team overview', maxlength: 100 });
    const cols = h('select', { 'aria-label': 'Columns' }, [1, 2, 3].map(n => h('option', { value: n }, n + (n === 1 ? ' column' : ' columns'))));
    cols.value = 2;
    const shared = h('input', { type: 'checkbox', 'aria-label': 'Share with the organization' });
    formDialog('New dashboard', [field('Name', name), field('Columns', cols), h('label', { class: 'cf-row' }, shared, ' Share with the organization')], 'Create dashboard',
      () => api('POST', '/api/v1/dashboards', { name: name.value, columns: Number(cols.value), shared: shared.checked ? 'org' : 'private' }),
      d => { location.hash = '#/dashboards/' + d.id; });
  }
  async function userDialog() {
    const email = h('input', { type: 'email', 'aria-label': 'Email', placeholder: 'name@example.com' });
    const name = h('input', { type: 'text', 'aria-label': 'Full name' });
    const pw = h('input', { type: 'password', 'aria-label': 'Initial password', autocomplete: 'new-password' });
    const admin = h('input', { type: 'checkbox', 'aria-label': 'Administrator' });
    const org = h('select', { 'aria-label': 'Organization' });
    const fields = [field('Email', email), field('Full name', name), field('Initial password', pw)];
    if (S.me && S.me.admin) { // the instance administrator picks the organization; an organization administrator adds to their own
      const r = await api('GET', '/api/v1/orgs');
      for (const o of (r.status === 200 && r.data) || []) org.append(h('option', { value: o.id }, o.name));
      if (org.children.length) fields.push(field('Organization', org));
    }
    fields.push(h('label', { class: 'cf-row' }, admin, ' Administrator'));
    formDialog('New user', fields, 'Create user',
      () => api('POST', '/api/v1/users', { email: email.value, name: name.value, password: pw.value, admin: admin.checked, org: org.value || '' }),
      u => toast('User ' + (u.email || email.value) + ' created'));
  }
  function organizationDialog() {
    const name = h('input', { type: 'text', 'aria-label': 'Organization name', placeholder: 'Acme Ltd', maxlength: 120 });
    const email = h('input', { type: 'email', 'aria-label': 'Administrator email' });
    const aname = h('input', { type: 'text', 'aria-label': 'Administrator name' });
    const pw = h('input', { type: 'password', 'aria-label': 'Administrator initial password', autocomplete: 'new-password' });
    formDialog('New organization', [field('Name', name), field('Administrator email', email), field('Administrator name', aname), field('Administrator initial password', pw)], 'Create organization',
      () => api('POST', '/api/v1/orgs', { name: name.value, admin_email: email.value, admin_name: aname.value, admin_password: pw.value }),
      o => toast('Organization ' + (o.name || name.value) + ' created'));
  }

  // One file to an issue: the ordinary request up to 8 MB; above that straight to the object store when the installation has one
  // (direct returns null when it has not), else in resumable chunks. Reports why it failed with a toast and returns false.
  async function uploadOne(key, f) {
    if (f.size <= 8 << 20) {
      const fd = new FormData(); fd.append('file', f, f.name || 'pasted.png');
      const r = await fetch('/api/v1/issues/' + key + '/attachments', { method: 'POST', body: fd, credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } });
      if (r.status === 201) return true;
      toast((await r.json().catch(() => ({}))).error || 'Upload failed'); return false;
    }
    const d = await uploadDirect(key, f);
    return d === null ? uploadResumable(key, f) : d;
  }
  // Direct upload (S3 backend): presigned PUTs, three parts at a time, each retried; the server verifies at completion.
  async function uploadDirect(key, f) {
    const s = await api('POST', '/api/v1/issues/' + key + '/uploads/direct', { name: f.name, size: f.size, type: f.type });
    if (s.status === 400 && /ordinary upload/.test((s.data && s.data.error) || '')) return null;
    if (s.status !== 201) { toast((s.data && s.data.error) || 'Upload failed'); return false; }
    const du = s.data;
    const put = async (url, body) => {
      for (let i = 0; i < 3; i++) {
        try { const r = await fetch(url, { method: 'PUT', body }); if (r.ok) return r; } catch (e) { /* retry */ }
        await new Promise(r => setTimeout(r, 500 * (i + 1)));
      }
      throw new Error('the store did not accept the upload');
    };
    let parts = [];
    try {
      if (du.mode === 'single') await put(du.url, f);
      else {
        const queue = du.parts.slice(); parts = [];
        const worker = async () => {
          for (let p = queue.shift(); p; p = queue.shift()) {
            const r = await put(p.url, f.slice((p.n - 1) * du.part_size, p.n * du.part_size));
            const etag = r.headers.get('ETag');
            if (!etag) throw new Error('the bucket must expose the ETag header to browsers (CORS)');
            parts.push({ n: p.n, etag });
          }
        };
        await Promise.all([worker(), worker(), worker()]);
      }
    } catch (e) {
      await api('DELETE', '/api/v1/uploads/direct/' + du.id);
      toast('Upload interrupted: ' + e.message);
      return false;
    }
    const c = await api('POST', '/api/v1/uploads/direct/' + du.id + '/complete', { parts });
    if (c.status !== 201) { toast((c.data && c.data.error) || 'Upload failed'); return false; }
    return true;
  }
  async function uploadResumable(key, f) {
    const s = await api('POST', '/api/v1/issues/' + key + '/uploads', { name: f.name, size: f.size, type: f.type });
    if (s.status !== 201) { toast(s.data.error || 'Upload failed'); return false; }
    const id = s.data.id, CH = 8 << 20; let off = 0, fails = 0;
    while (off < f.size) {
      try {
        const r = await fetch('/api/v1/uploads/' + id, { method: 'PATCH', credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane', 'Upload-Offset': String(off) }, body: f.slice(off, off + CH) });
        if (r.status === 409) off = Number(r.headers.get('Upload-Offset')); else if (r.ok) off = Number(r.headers.get('Upload-Offset')); else throw new Error('bad');
        fails = 0;
      } catch (e) { // connection dropped: ask where the server stopped and resume from there
        if (++fails > 5) { toast('Upload interrupted; try again'); return false; }
        const hd = await fetch('/api/v1/uploads/' + id, { method: 'HEAD', credentials: 'same-origin' }).catch(() => null);
        if (hd && hd.ok) off = Number(hd.headers.get('Upload-Offset')); else await new Promise(r => setTimeout(r, 1000 * fails));
      }
    }
    const c = await api('POST', '/api/v1/uploads/' + id + '/commit');
    if (c.status !== 201) { toast(c.data.error || 'Upload failed'); return false; }
    return true;
  }

  function createDialog() {
    const proj = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key }, p.key + ' · ' + p.name)));
    const type = h('select', { 'aria-label': 'Type' }, ['Task', 'Bug', 'Story', 'Epic'].map(t => h('option', {}, t)));
    const sum = h('input', { type: 'text', placeholder: 'Summary', 'aria-label': 'Summary', style: 'width:100%' });
    const desc = h('textarea', { placeholder: 'Description', 'aria-label': 'Description' }), msg = h('div', { class: 'err', role: 'alert' });
    const cfBox = h('div', { class: 'cf-box' });
    // files to attach: chosen with the button, dropped anywhere on the dialog, or pasted; uploaded to the issue once it exists
    const files = [], chips = h('div', { class: 'att-pending', role: 'list', 'aria-label': 'Files to attach' });
    const size = n => n < 1024 ? n + ' B' : n < 1 << 20 ? (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB' : n < 1 << 30 ? (n / (1 << 20)).toFixed(1) + ' MB' : (n / (1 << 30)).toFixed(1) + ' GB';
    function showFiles() {
      clear(chips);
      for (const f of files) chips.append(h('span', { class: 'chip', role: 'listitem' }, h('span', { class: 'att-name' }, f.name || 'pasted file'), ' ', h('span', { class: 'muted' }, size(f.size)),
        h('button', { class: 'btn ghost', type: 'button', 'aria-label': 'Remove ' + (f.name || 'pasted file'), onclick: () => { files.splice(files.indexOf(f), 1); showFiles(); } }, '×')));
    }
    function addFiles(list) {
      for (const f of list) if (!files.some(g => g.name === f.name && g.size === f.size && g.lastModified === f.lastModified)) files.push(f);
      showFiles();
    }
    const pick = h('input', { type: 'file', multiple: true, class: 'sr', id: 'create-files', 'aria-label': 'Attach files' });
    pick.addEventListener('change', () => { addFiles([...pick.files]); pick.value = ''; });
    const attach = h('div', { class: 'attach-row' }, pick, h('label', { for: 'create-files', class: 'btn' }, '📎 Attach files'), h('span', { class: 'muted' }, ' or drop files here'), chips);
    let cfCtl = [], form = { rules: [], role: 'member' };
    const descCtl = { el: desc, get: () => desc.value.trim() || null, star: null };
    const values = () => { const vals = { summary: sum.value }; for (const [f, c] of cfCtl) vals[f.key] = c.get(); return vals; };
    // required and hidden follow the rules as the values change
    function applyRules() {
      const vals = values();
      for (const [f, c] of cfCtl) {
        const st = ruleState(form.rules, f, vals, form.role), row = c.row;
        if (row) row.hidden = st.hidden;
        if (c.star) c.star.textContent = f.name + (st.required ? ' *' : '');
        c.el.setAttribute('aria-required', st.required ? 'true' : 'false');
      }
    }
    async function loadFields() {
      const r = await api('GET', '/api/v1/projects/' + proj.value + '/form?op=create&type=' + encodeURIComponent(type.value));
      clear(cfBox); cfCtl = [];
      if (r.status !== 200) return;
      form = r.data;
      for (const tab of form.tabs) {
        const box = h('fieldset', { class: 'cf-tab' }, h('legend', {}, tab.name));
        let any = false;
        for (const f of tab.fields) {
          if (f.key === 'description') { cfCtl.push([f, descCtl]); desc.hidden = !!f.hidden; continue; } // the description has its own box above
          const c = fieldControl(f, f.default, false);
          const row = fieldRow(f, c); c.row = row;
          c.el.addEventListener('change', applyRules); c.el.addEventListener('input', applyRules);
          cfCtl.push([f, c]); box.append(row); any = true;
        }
        if (any) cfBox.append(box);
      }
      applyRules();
    }
    proj.addEventListener('change', loadFields); type.addEventListener('change', loadFields); sum.addEventListener('input', applyRules);
    loadFields();
    const close = modal(h('div', {}, h('h2', {}, 'Create issue'), h('div', { class: 'chips' }, proj, type), sum, h('div', { style: 'height:8px' }), desc, cfBox, attach, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
        h('button', { class: 'btn primary', id: 'create-go', onclick: async () => {
          const body = { project: proj.value, type: type.value, summary: sum.value, description: desc.value, custom: {} };
          const vals = values();
          for (const [f, c] of cfCtl) {
            if (ruleState(form.rules, f, vals, form.role).hidden) continue;
            const x = c.get(); if (x == null) continue;
            if (f.key === 'priority' || f.key === 'assignee' || f.key === 'labels') body[f.key] = x; else if (!f.builtin) body.custom[f.key] = x;
          }
          const go = document.getElementById('create-go'); go.disabled = true;
          const r = await api('POST', '/api/v1/issues', body);
          if (r.status !== 201) { msg.textContent = r.data.error; go.disabled = false; return; }
          const failed = [];
          for (const [i, f] of files.entries()) { msg.textContent = 'Uploading ' + (i + 1) + ' of ' + files.length + ': ' + f.name; if (!await uploadOne(r.data.key, f)) failed.push(f.name); }
          close(); location.hash = '#/issue/' + r.data.key;
          if (failed.length) toast(r.data.key + ' was created, but these files did not upload: ' + failed.join(', ') + '. Attach them on the issue.');
        } }, 'Create'))));
    const dlg = close.scrim.firstChild, hasFiles = e => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
    close.scrim.addEventListener('dragover', e => { if (hasFiles(e)) { e.preventDefault(); dlg.classList.add('dropping'); } });
    close.scrim.addEventListener('dragleave', e => { if (!close.scrim.contains(e.relatedTarget)) dlg.classList.remove('dropping'); });
    close.scrim.addEventListener('drop', e => { if (!hasFiles(e)) return; e.preventDefault(); dlg.classList.remove('dropping'); addFiles([...e.dataTransfer.files]); });
    close.scrim.addEventListener('paste', e => { const fs = [...(e.clipboardData ? e.clipboardData.files : [])]; if (fs.length) { e.preventDefault(); addFiles(fs); } });
    sum.focus();
  }

  // A lifecycle operation: show the plan first (a dry run changes nothing), apply only when the person confirms.
  function lifecycleDialog(key, op, title, controls, build, onDone) {
    const out = h('div', { 'aria-live': 'polite' }), err = h('div', { class: 'err', role: 'alert' });
    const apply = h('button', { class: 'btn primary', disabled: true }, 'Apply');
    let latest = 0;
    async function refresh() {
      const n = ++latest;
      clear(out).append(h('span', { class: 'muted' }, 'Planning…'));
      apply.disabled = true;
      const body = Object.assign({ op, dry_run: true }, build());
      const r = await api('POST', '/api/v1/issues/' + key + '/lifecycle', body);
      if (n !== latest) return; // a newer plan is on its way
      clear(out);
      const plan = r.status === 200 ? r.data : (r.data && r.data.detail && r.data.detail.op ? r.data.detail : null);
      if (!plan) { out.append(h('div', { class: 'err' }, (r.data && r.data.error) || 'Could not plan')); return; }
      const list = (label, items, cls) => items && items.length ? h('div', { class: cls || '' }, h('strong', {}, label), h('ul', {}, items.map(i => h('li', {}, i)))) : null;
      const counts = Object.entries(plan.counts || {}).map(([k2, v]) => k2.replace(/_/g, ' ') + ': ' + v).join(' · ');
      out.append(list('Blocked', plan.blockers, 'err'), list('This will', plan.changes), list('Dropped', plan.drops), list('Warnings', plan.warnings), counts ? h('div', { class: 'muted' }, counts) : null);
      apply.disabled = plan.blockers.length > 0;
    }
    apply.addEventListener('click', async () => {
      apply.disabled = true;
      const r = await api('POST', '/api/v1/issues/' + key + '/lifecycle', Object.assign({ op, expect_key: key }, build()));
      if (r.status === 200) { close(); onDone(r.data); } else { err.textContent = (r.data && r.data.error) || 'Failed'; apply.disabled = false; }
    });
    const close = modal(h('div', {}, h('h2', {}, title), h('div', { class: 'chips' }, controls.map(c => { c.addEventListener('change', refresh); return c; })), out, err,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), apply)));
    refresh();
  }

  function modal(content) {
    const prev = document.activeElement;
    const scrim = h('div', { class: 'scrim', onmousedown: e => { if (e.target === scrim) close(); } }, h('div', { class: 'dialog', role: 'dialog', 'aria-modal': 'true' }, content));
    function close() { scrim.remove(); document.removeEventListener('keydown', key); if (prev && prev.focus) prev.focus(); }
    function key(e) {
      if (e.key === 'Escape' && !scrim.dataset.sticky) close();
      if (e.key === 'Tab') { // focus trap
        const f = [...scrim.querySelectorAll('button,input,textarea,select,a[href]')].filter(x => !x.disabled);
        if (!f.length) return;
        const first = f[0], last = f[f.length - 1];
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      }
    }
    const dlgEl = scrim.firstChild, hd = dlgEl.querySelector('h1,h2,h3'); // the dialog is named by its heading
    if (hd) { hd.id = hd.id || 'dlg-' + Math.random().toString(36).slice(2, 8); dlgEl.setAttribute('aria-labelledby', hd.id); } else dlgEl.setAttribute('aria-label', 'Dialog');
    document.addEventListener('keydown', key);
    document.body.append(scrim);
    const first = scrim.querySelector('input,textarea,button'); if (first) first.focus();
    close.scrim = scrim;
    return close;
  }

  // ---------- ticket thread ----------
  const draftKey = (key, f) => 'tl-draft:' + S.me.id + ':' + key + ':' + f;
  // A draft of a very large text is kept in pieces (PERF-09): the part before the window being edited and the part after it in chunks of 16 KB, and the window
  // in one key of its own, so that typing rewrites only the small one; the main key says how many chunks there are. Anything else is one key with the whole draft.
  const CHUNK = 16384;
  const partKey = (key, f, p) => draftKey(key, f) + ':' + p;
  function localDraft(key, f) {
    try {
      const d = JSON.parse(localStorage.getItem(draftKey(key, f)));
      if (d && d.parts) {
        const read = (name, n) => { let out = ''; for (let i = 0; i < n; i++) { const x = localStorage.getItem(partKey(key, f, name + i)); if (x == null) return null; out += x; } return out; };
        const hh = read('h', d.hn), t = read('t', d.tn), m = localStorage.getItem(partKey(key, f, 'm'));
        return hh == null || t == null || m == null ? null : { body: hh + m + t, base_seq: d.base_seq, at: d.at };
      }
      return d;
    } catch (e) { return null; }
  }
  function setLocalDraft(key, f, v) {
    try {
      const prefix = draftKey(key, f) + ':';
      for (const k of Object.keys(localStorage)) if (k.startsWith(prefix)) localStorage.removeItem(k);
      if (v) localStorage.setItem(draftKey(key, f), JSON.stringify(v)); else localStorage.removeItem(draftKey(key, f));
    } catch (e) { /* storage unavailable */ }
  }
  // The writer of a windowed draft: stage() queues the chunks of the outside parts that differ from what is stored, pump() writes one per timer tick (each a
  // couple of milliseconds, not one block of a megabyte), drain() writes what is left at once, and commit() writes the window and the main key.
  function draftWriter(key, f) {
    const w = { last: {}, queue: [], counts: { h: 0, t: 0 }, timer: 0 };
    const put = ([k, c]) => { try { if (c === null) { localStorage.removeItem(k); delete w.last[k]; } else { localStorage.setItem(k, c); w.last[k] = c; } } catch (e) { /* storage unavailable */ } };
    w.stage = (name, str) => {
      const n = Math.max(1, Math.ceil(str.length / CHUNK));
      for (let i = 0; i < n; i++) { const c = str.slice(i * CHUNK, (i + 1) * CHUNK), k = partKey(key, f, name + i); if (w.last[k] !== c) w.queue.push([k, c]); }
      for (let i = n; i < w.counts[name]; i++) w.queue.push([partKey(key, f, name + i), null]);
      w.counts[name] = n;
      w.pump();
    };
    w.pump = () => { if (w.timer || !w.queue.length) return; w.timer = setTimeout(() => { w.timer = 0; if (w.queue.length) put(w.queue.shift()); w.pump(); }, 0); };
    w.drain = () => { clearTimeout(w.timer); w.timer = 0; while (w.queue.length) put(w.queue.shift()); };
    w.commit = (mid, meta) => {
      w.drain();
      try { localStorage.setItem(partKey(key, f, 'm'), mid); localStorage.setItem(draftKey(key, f), JSON.stringify({ parts: true, hn: w.counts.h, tn: w.counts.t, base_seq: meta.base_seq, at: meta.at })); } catch (e) { /* storage unavailable */ }
    };
    return w;
  }

  // Prev/next in a ticket (PERF-08): while a ticket is open the two neighbours in the list it was opened from are fetched in the background and kept
  // for a short while, so moving to one shows it at once from the page's own memory; the page revalidates it as it does any open ticket. An entry is
  // used once (a second visit asks the server again) and expires after a minute, so what is shown can only be a little behind, never invented.
  const VCACHE_MS = 60000;
  function prefetchView(key) {
    const hit = S.vcache.get(key);
    if (hit && (hit.data || Date.now() - hit.at < VCACHE_MS)) return;
    S.vcache.set(key, { at: Date.now(), data: null });
    api('GET', '/api/v1/issues/' + key + '/view').then(r => {
      if (r.status === 200) S.vcache.set(key, { at: Date.now(), data: r.data }); else S.vcache.delete(key);
      while (S.vcache.size > 12) S.vcache.delete(S.vcache.keys().next().value);
    }).catch(() => S.vcache.delete(key));
  }
  function takeView(key) {
    const hit = S.vcache.get(key);
    S.vcache.delete(key);
    return hit && hit.data && Date.now() - hit.at < VCACHE_MS ? hit.data : null;
  }
  function neighbours(key) {
    const i = S.nav.keys.indexOf(key);
    return i < 0 ? {} : { prev: S.nav.keys[i - 1], next: S.nav.keys[i + 1], at: i + 1, of: S.nav.keys.length };
  }

  async function issueView(requestedKey) {
    let key = requestedKey;
    stopTimers();
    const root = h('div', { class: 'page fluid issue-page' });
    clear($app).append(topbar(), root);
    let v = await load();
    async function load() {
      const cached = takeView(key);
      if (cached) return cached;
      const r = await api('GET', '/api/v1/issues/' + key + '/view');
      if (r.status === 410) { clear(root).append(h('div', { class: 'banner danger', role: 'alert' }, 'This issue was deleted. ', h('a', { href: '#/trash/' + key.split('-')[0] }, 'Open the trash'))); return null; }
      if (r.status === 404) { clear(root).append(h('p', {}, 'Issue not found, or you do not have access.')); return null; }
      return r.data;
    }
    if (!v) return;
    const st = { v, tab: 'description', editing: null, dirty: false, saveId: null, serverTimer: 0, localTimer: 0, changedBy: null, deleted: null };
    if (v.alias && v.issue.key !== key) { key = v.issue.key; history.replaceState(null, '', '#/issue/' + key); } // an old key redirects to the current one
    S.view = st;
    render();

    function pager(k) {
      const n = neighbours(k);
      if (n.of == null) return null;
      for (const x of [n.prev, n.next]) if (x) prefetchView(x);
      const go = to => { if (to) location.hash = '#/issue/' + to; };
      return h('span', { class: 'chips', style: 'margin:0', role: 'group', 'aria-label': 'Previous and next in the list' },
        h('button', { class: 'btn ghost', id: 'prev-issue', disabled: !n.prev, onclick: () => go(n.prev), 'aria-label': 'Previous issue in the list' }, '‹'),
        h('span', { class: 'muted' }, n.at + ' of ' + n.of),
        h('button', { class: 'btn ghost', id: 'next-issue', disabled: !n.next, onclick: () => go(n.next), 'aria-label': 'Next issue in the list' }, '›'));
    }

    function canEdit() { return !v.mirror && (v.role === 'member' || v.role === 'admin'); } // a mirrored issue is edited in Jira

    function render() {
      const is = v.issue;
      clear(root);
      // header: presence stack
      root.append(h('div', { class: 'chips' },
        h('button', { class: 'btn ghost', onclick: () => history.back(), 'aria-label': 'Back' }, '←'),
        h('span', { class: 'key' }, is.key), pager(is.key), h('span', { class: 'grow' }), h('span', { id: 'pres', class: 'chips', style: 'margin:0' }), viewControls(false),
        h('button', { class: 'btn', id: 'watch-btn', 'aria-pressed': 'false' }, 'Watch'),
        canEdit() ? h('button', { class: 'btn', 'aria-haspopup': 'dialog', onclick: actionsMenu }, 'Actions ▾') : null));
      if (st.deleted) root.append(h('div', { class: 'banner danger', role: 'alert' }, 'This issue was deleted' + (st.deleted.by ? ' by ' + st.deleted.by : '') + '. Anything you were typing stays in your draft. ', h('a', { href: '#/trash/' + is.project }, 'Open the trash')));
      fillPresence();
      fillWatch();
      if (v.mirror) {
        const m = v.mirror, ago = m.synced_at ? L.relTime(m.synced_at) : 'not yet';
        root.append(h('div', { class: 'banner ' + (m.stale ? 'warn' : 'info'), role: 'status' },
          'Mirrored from Jira and read-only here. Synced ' + ago + (m.stale && m.error ? ' (the last sync failed: ' + m.error + ')' : '') + '. ',
          h('a', { href: m.site_url.replace(/\/$/, '') + '/browse/' + encodeURIComponent(m.issue_key || is.key), rel: 'noopener noreferrer', target: '_blank' }, 'Edit in Jira')));
      }
      if (is.archived) {
        root.append(h('div', { class: 'banner info', role: 'status' }, 'Archived ' + L.relTime(is.archived) + ' and read-only. ',
          h('button', { class: 'btn', onclick: async () => { const r = await api('DELETE', '/api/v1/issues/' + key + '/archive'); if (r.status === 200) { toast('Restored from the archive.'); await reloadAll(); } else toast((r.data && r.data.error) || 'Could not restore'); } }, 'Unarchive')));
      }
      if (v.request) root.append(h('div', { class: 'banner info', role: 'status' }, 'Raised through the portal by ' + (v.request.customer || v.request.email) + ' (' + v.request.email + ')' + (v.request.request_type ? ' · ' + v.request.request_type : '') + (v.request.company ? ' · ' + v.request.company : '') + (v.request.shared_with_company ? ' (shared with the whole company)' : '') + (v.request.shared_with && v.request.shared_with.length ? ' · also seen by ' + v.request.shared_with.join(', ') : '') + '. A comment that is not internal is visible to the customer' + ((v.request.shared_with && v.request.shared_with.length) || v.request.shared_with_company ? 's it is shared with' : '') + '.'));
      if (v.sla && v.sla.length) root.append(slaEl(v.sla));
      // the title and the description first; comments and history are tabs; the fields sit in a column beside them
      const tabs = [['description', 'Description'], ['comments', 'Comments (' + v.comments.length + ')'], ['history', 'History']];
      const pick = id => { st.tab = id; render(); };
      const bar = h('div', { class: 'tabs issue-tabs', role: 'tablist', 'aria-label': 'Issue sections' }, tabs.map(([id, label]) =>
        h('button', { class: 'tab', role: 'tab', id: 'tab-' + id, 'aria-selected': String(st.tab === id), 'aria-controls': 'panel-' + id, tabindex: st.tab === id ? 0 : -1,
          onclick: () => pick(id),
          onkeydown: e => {
            const i = tabs.findIndex(t => t[0] === st.tab), to = e.key === 'ArrowRight' ? tabs[(i + 1) % tabs.length] : e.key === 'ArrowLeft' ? tabs[(i + tabs.length - 1) % tabs.length] : null;
            if (to) { e.preventDefault(); pick(to[0]); document.getElementById('tab-' + to[0]).focus(); }
          } }, label)));
      const panel = h('div', { role: 'tabpanel', id: 'panel-' + st.tab, 'aria-labelledby': 'tab-' + st.tab });
      if (st.tab === 'description') panel.append(descriptionEl(is));
      else if (st.tab === 'comments') panel.append(threadEl(), replyEl());
      else panel.append(historyEl());
      root.append(h('div', { class: 'issue-layout' },
        h('div', { class: 'issue-main' }, subjectEl(is), bar, panel),
        h('aside', { class: 'issue-side', 'aria-label': 'Details' }, chipLine(is), customFieldsEl(is), attachmentsEl(), developmentEl(), approvalsEl())));
    }

    // ----- service level clocks of a request
    function slaEl(clocks) {
      const mins = n => { n = Math.abs(n); const d = Math.floor(n / 1440), hh = Math.floor((n % 1440) / 60), m = n % 60; return (d ? d + 'd ' : '') + (hh ? hh + 'h ' : '') + (m || (!d && !hh) ? m + 'm' : ''); };
      const name = { first_response: 'First response', resolution: 'Resolution' };
      return h('div', { class: 'chips', role: 'group', 'aria-label': 'Service level goals' }, clocks.map(c => {
        const text = c.state === 'met' ? 'met' + (c.late ? ' late' : '') : c.state === 'paused' ? 'paused' : c.state === 'breached' ? 'missed' : mins(c.remaining_minutes) + ' left';
        const bad = c.state === 'breached' || c.late;
        return h('span', { class: 'badge', style: bad ? 'background:var(--danger,#c0392b);color:#fff' : '', title: 'Goal ' + mins(c.goal_minutes) + (c.due ? ', due ' + new Date(c.due).toLocaleString() : '') }, name[c.kind] + ': ' + text);
      }));
    }

    // ----- approvals: who was asked, what they said, and (for an approver) the buttons
    function approvalsEl() {
      const list = v.approvals || [];
      const pending = list.some(a => a.status === 'pending');
      const box = h('section', { class: 'card', 'aria-label': 'Approvals', style: 'padding:10px 12px;margin:12px 0' });
      if (!list.length && !canEdit()) return h('span', { hidden: true });
      box.append(h('h3', { style: 'margin:0 0 6px' }, 'Approvals'));
      const word = { pending: 'Waiting', granted: 'Approved', rejected: 'Rejected', cancelled: 'Withdrawn' };
      for (const ap of list) {
        const rule = ap.rule === 'all' ? 'everyone must approve' : ap.rule === 'any' ? 'one approval is enough' : ap.needed + ' of ' + ap.approvers.length + ' must approve';
        const row = h('div', { class: 'row', style: 'flex-direction:column;align-items:stretch;gap:4px' },
          h('div', {}, h('strong', {}, word[ap.status] || ap.status), ' ', h('span', { class: 'muted' }, rule + (ap.requested_by ? ' · asked by ' + ap.requested_by.name : '') + (ap.deadline ? ' · due ' + new Date(ap.deadline).toLocaleString() : '')), ap.overdue ? h('span', { class: 'badge', style: 'margin-left:6px' }, 'Overdue') : null),
          ap.comment ? h('div', { class: 'muted' }, ap.comment) : null,
          h('ul', { style: 'margin:0;padding-left:18px' }, ap.approvers.map(p => h('li', {}, p.user.name + ' — ' + (p.decision === 'approve' ? 'approved' : p.decision === 'reject' ? 'rejected' : 'not decided yet') + (p.comment ? ': ' + p.comment : '')))));
        if (ap.mine) {
          const c = h('input', { type: 'text', 'aria-label': 'Comment on your decision', placeholder: 'Comment (optional)', style: 'min-width:220px' });
          const act = d => async () => { const r = await api('POST', '/api/v1/approvals/' + ap.id + '/decision', { decision: d, comment: c.value }); if (r.status === 200) await reloadAll(); else toast((r.data && r.data.error) || 'Could not record the decision'); };
          row.append(h('div', { class: 'chips' }, c, h('button', { class: 'btn primary', onclick: act('approve') }, 'Approve'), h('button', { class: 'btn', onclick: act('reject') }, 'Reject')));
        }
        if (ap.status === 'pending' && canEdit()) row.append(h('div', {}, h('button', { class: 'btn ghost', onclick: async () => { const r = await api('POST', '/api/v1/approvals/' + ap.id + '/cancel'); if (r.status === 200) await reloadAll(); else toast((r.data && r.data.error) || 'Could not withdraw it'); } }, 'Withdraw the request')));
        box.append(row);
      }
      if (canEdit() && !pending) {
        const who = h('input', { type: 'text', 'aria-label': 'Approvers', placeholder: 'ana@example.com, group:Release managers, reporter', style: 'min-width:min(300px,100%)' });
        const rule = h('select', { 'aria-label': 'Approval rule' }, h('option', { value: 'all' }, 'Everyone'), h('option', { value: 'any' }, 'Any one'), h('option', { value: 'n' }, 'At least N'));
        const n = h('input', { type: 'number', 'aria-label': 'How many must approve', min: 1, value: 2, style: 'width:70px', hidden: true });
        rule.addEventListener('change', () => { n.hidden = rule.value !== 'n'; });
        const due = h('input', { type: 'datetime-local', 'aria-label': 'Decision due' });
        const note = h('input', { type: 'text', 'aria-label': 'Note to the approvers', placeholder: 'Note (optional)' });
        box.append(h('div', { class: 'chips' }, who, rule, n, due, note, h('button', { class: 'btn', onclick: async () => {
          const body = { users: [], groups: [], derived: [], rule: rule.value, n: Number(n.value), comment: note.value };
          for (const t of who.value.split(',').map(x => x.trim()).filter(Boolean)) {
            if (/^group:/i.test(t)) body.groups.push(t.slice(6).trim()); else if (/^(reporter|assignee)$/i.test(t)) body.derived.push(t.toLowerCase()); else body.users.push(t);
          }
          if (due.value) body.deadline = new Date(due.value).toISOString();
          const r = await api('POST', '/api/v1/issues/' + key + '/approvals', body);
          if (r.status === 201) await reloadAll(); else toast((r.data && r.data.error) || 'Could not ask for approval');
        } }, 'Ask for approval')));
      }
      return box;
    }

    function noteFor(field, label) {
      const box = h('span', { 'data-note': field, 'data-label': label || '' });
      fillNote(box);
      return box;
    }
    function fillNote(box) {
      clear(box);
      const field = box.dataset.note, label = box.dataset.label;
      const e = (v.presence || []).find(p => p.editing === field);
      if (e) box.append(h('span', { class: 'note', role: 'status' }, e.name + ' is editing' + (label ? ' the ' + label : '') + ' (' + Math.max(1, Math.round((e.editing_seconds || 0) / 60)) + ' min)'));
    }
    function fillPresence() {
      const box = document.getElementById('pres');
      if (!box) return;
      clear(box);
      const others = v.presence || [];
      if (others.length) box.append(h('span', { class: 'stack', 'aria-label': others.length + ' others here' }, others.map(o => avatar(o.name, o.editing ? presColor(o.user_id) : ''))),
        h('span', { class: 'muted' }, others.map(o => o.name).join(', ') + (others.length > 1 ? ' are here' : ' is here')));
    }
    // presence arrives while someone is typing: update the chrome in place, never touch the editor text
    function updatePresenceUI() {
      fillPresence();
      document.querySelectorAll('[data-note]').forEach(fillNote);
      const warn = document.getElementById('editing-warning');
      if (warn) { const o = (v.presence || []).find(p => p.editing === 'description'); clear(warn); if (o) warn.append(h('div', { class: 'banner warn' }, o.name + ' is also editing this field. You can continue; if you both save you may need to merge.')); }
    }

    // ----- lifecycle actions: clone, move, convert, delete
    function actionsMenu() {
      const is = v.issue;
      const close = modal(h('div', {}, h('h2', {}, 'Actions for ' + is.key),
        h('div', { class: 'chips', style: 'flex-direction:column;align-items:stretch' },
          h('button', { class: 'btn', onclick: () => { close(); cloneDialog(); } }, 'Clone…'),
          h('button', { class: 'btn', onclick: () => { close(); moveDialog(); } }, 'Move to another project…'),
          h('button', { class: 'btn', onclick: () => { close(); convertDialog(); } }, 'Change issue type…'),
          is.status_category === 'done' && !is.archived ? h('button', { class: 'btn', onclick: async () => { close(); const r = await api('POST', '/api/v1/issues/' + key + '/archive', undefined, WANT_UNDO); if (r.status === 200) { undoToast(key + ' archived. It is hidden from boards and searches.', r, () => reloadAll()); await reloadAll(); } else toast((r.data && r.data.error) || 'Could not archive'); } }, 'Archive') : null,
          h('button', { class: 'btn', onclick: () => { close(); deleteDialog(); } }, 'Delete…')),
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Close'))));
    }
    const projectSelect = (exclude, label) => h('select', { 'aria-label': label }, S.projects.filter(p => p.key !== exclude).map(p => h('option', { value: p.key }, p.key + ' · ' + p.name)));
    function cloneDialog() {
      const proj = projectSelect('', 'Target project'); proj.value = v.issue.project;
      const cb = (id, text, on) => h('label', { style: 'display:flex;gap:6px;align-items:center' }, h('input', { type: 'checkbox', id, checked: on }), text);
      const att = cb('c-att', 'Attachments', true), subs = cb('c-sub', 'Subtasks', true), com = cb('c-com', 'Comments', false), lnk = cb('c-lnk', 'Links', false);
      lifecycleDialog(key, 'clone', 'Clone ' + v.issue.key, [proj, att, subs, com, lnk],
        () => ({ clone: { project: proj.value, attachments: att.querySelector('input').checked, subtasks: subs.querySelector('input').checked, comments: com.querySelector('input').checked, links: lnk.querySelector('input').checked } }),
        r => { location.hash = '#/issue/' + r.result_key; });
    }
    function moveDialog() {
      const proj = projectSelect(v.issue.project, 'Target project');
      if (!proj.options.length) { toast('There is no other project to move to.'); return; }
      lifecycleDialog(key, 'move', 'Move ' + v.issue.key, [proj], () => ({ project: proj.value }), r => { toast('Moved to ' + r.result_key + '. The old key keeps working.'); location.hash = '#/issue/' + r.result_key; });
    }
    function convertDialog() {
      const type = h('select', { 'aria-label': 'New type' }, ['Epic', 'Story', 'Task', 'Bug', 'Subtask'].filter(t => t !== v.issue.type).map(t => h('option', {}, t)));
      lifecycleDialog(key, 'convert', 'Change the type of ' + v.issue.key, [type], () => ({ type: type.value }), () => { toast('Type changed'); reloadAll(); });
    }
    // a delete happens at once with Undo when its plan has nothing blocking and drops nothing; otherwise the plan is shown first (UNDO-06)
    async function deleteDialog() {
      const dry = await api('POST', '/api/v1/issues/' + key + '/lifecycle', { op: 'delete', dry_run: true });
      const plan = dry.status === 200 ? dry.data : null;
      if (!plan || plan.blockers.length || plan.drops.length) {
        lifecycleDialog(key, 'delete', 'Delete ' + v.issue.key, [], () => ({}), () => { toast('Moved to the trash. It can be restored for 30 days.'); location.hash = '#/'; });
        return;
      }
      const r = await api('POST', '/api/v1/issues/' + key + '/lifecycle', { op: 'delete', expect_key: key }, WANT_UNDO);
      if (r.status !== 200) { toast((r.data && r.data.error) || 'Could not delete'); return; }
      const k = v.issue.key;
      undoToast(k + ' moved to the trash.', r, () => { location.hash = '#/issue/' + k; });
      location.hash = '#/';
    }

    // ----- watching: the button shows whether you hear about this issue and flips your explicit choice
    async function fillWatch() {
      const b = document.getElementById('watch-btn');
      if (!b) return;
      const r = await api('GET', '/api/v1/issues/' + key + '/watch');
      if (r.status !== 200) { b.hidden = true; return; }
      const paint = w => {
        b.textContent = w.watching ? 'Watching ✓' : 'Watch';
        b.setAttribute('aria-pressed', String(w.watching));
        b.title = w.watchers + ' watching';
        b.onclick = async () => { const x = await api('PUT', '/api/v1/issues/' + key + '/watch', { watch: !w.watching }); if (x.status === 200) paint(x.data); else toast((x.data && x.data.error) || 'Could not change watching'); };
      };
      paint(r.data);
    }

    // ----- subject (editable in place; same save protocol as the description)
    function subjectEl(is) {
      const inp = h('input', { class: 'subject-in', type: 'text', value: is.summary, 'aria-label': 'Summary', maxlength: 255, readonly: !canEdit() });
      const state = h('span', { class: 'state' });
      const commit = async () => {
        if (inp.value === v.issue.summary) return;
        const r = await saveText('summary', inp.value, v.issue.summary_seq, 'save');
        if (r.ok) { state.textContent = 'Saved ✓'; } else if (r.conflict) { await conflictDialog('summary', r.conflict, inp.value, () => reloadAll()); }
        else { state.textContent = r.msg; }
      };
      inp.addEventListener('blur', commit);
      inp.addEventListener('keydown', e => { if (e.key === 'Enter') inp.blur(); });
      inp.addEventListener('focus', () => heartbeat('summary'));
      return h('div', {}, noteFor('summary', 'summary'), h('h1', { class: 'subject' }, inp), state);
    }

    // ----- chip line: status transitions, priority, assignee, labels
    function chipLine(is) {
      const trans = h('select', { 'aria-label': 'Transition', disabled: !canEdit() || !v.transitions.length },
        h('option', { value: '' }, 'Transition…'), v.transitions.map(t => h('option', { value: t.id }, t.name + ' → ' + t.to)));
      trans.addEventListener('change', async () => {
        if (!trans.value) return;
        const r = await api('POST', '/api/v1/issues/' + key + '/transitions', { transition: trans.value, base_version: is.version }, WANT_UNDO);
        if (r.status === 200) { undoToast(key + ' moved to ' + r.data.status + '.', r, () => reloadAll()); await reloadAll(); } else scalarError(r, () => api('POST', '/api/v1/issues/' + key + '/transitions', { transition: trans.value }).then(reloadAll));
      });
      const pri = h('select', { 'aria-label': 'Priority', disabled: !canEdit() }, Object.keys(PRI).map(p => h('option', { value: p, selected: p === is.priority }, PRI[p] + ' ' + p)));
      pri.addEventListener('change', () => patch({ priority: pri.value }, 'priority'));
      const asg = h('select', { 'aria-label': 'Assignee', disabled: !canEdit() }, h('option', { value: '' }, 'Unassigned'),
        S.users.map(u => h('option', { value: u.id, selected: is.assignee && is.assignee.id === u.id }, u.name)));
      asg.addEventListener('change', () => patch({ assignee: asg.value }, 'assignee'));
      const labels = h('input', { type: 'text', value: (is.labels || []).join(' '), placeholder: 'labels', 'aria-label': 'Labels', disabled: !canEdit(), style: 'width:160px' });
      labels.addEventListener('change', () => patch({ labels: labels.value.split(/[\s,]+/).filter(Boolean) }, 'labels'));
      const vlist = h('datalist', { id: 'vers-list' });
      api('GET', '/api/v1/projects/' + is.project + '/versions').then(r => { if (r.status === 200) r.data.versions.filter(x => !x.archived).forEach(x => vlist.append(h('option', { value: x.name }))); });
      const verInput = (kind, label, vals) => {
        const el = h('input', { type: 'text', value: vals.join(', '), placeholder: label.toLowerCase(), 'aria-label': label, list: 'vers-list', disabled: !canEdit(), style: 'width:150px' });
        el.addEventListener('change', async () => {
          const r = await api('PUT', '/api/v1/issues/' + key + '/versions', { [kind]: el.value.split(/\s*,\s*/).filter(Boolean) });
          if (r.status === 200) await reloadAll(); else { toast((r.data && r.data.error) || 'Could not set versions'); el.value = vals.join(', '); }
        });
        return el;
      };
      return h('div', { class: 'chips' }, lozenge(is.status, is.status_category), trans, pri, asg, labels, vlist,
        verInput('fix', 'Fix versions', v.versions ? v.versions.fix : []), verInput('affects', 'Affects versions', v.versions ? v.versions.affects : []),
        h('span', { class: 'muted' }, is.type + ' · reported by ' + is.reporter.name));
    }
    async function patch(ch, field) {
      const r = await api('PATCH', '/api/v1/issues/' + key, Object.assign({ base_version: v.issue.version }, ch));
      if (r.status === 200) { await reloadAll(); return; }
      scalarError(r, () => api('PATCH', '/api/v1/issues/' + key, ch).then(reloadAll), field);
    }
    function scalarError(r, reapply, field) {
      if (r.status === 409 && r.data.code === 'version_conflict') {
        toast('Someone else changed ' + (r.data.detail.fields || [field]).join(', ') + '; your change was not applied.', { label: 'Reapply', fn: reapply });
      } else toast((r.data && r.data.error) || 'Could not save');
      reloadAll();
    }

    // ----- custom fields: each field saves on change through the same versioned PATCH as the built-ins
    function customFieldsEl(is) {
      const box = h('div', { class: 'card cf-card', style: 'padding:12px;margin:8px 0', hidden: true, 'aria-label': 'Custom fields' });
      // the form definition is cached so a re-render builds the controls synchronously (an async fill would replace a control the person is using)
      const ck = is.project + '/' + is.type, cache = S.fieldDefs || (S.fieldDefs = {});
      if (!cache[ck]) {
        if (!cache['pending:' + ck]) {
          cache['pending:' + ck] = true;
          api('GET', '/api/v1/projects/' + is.project + '/form?op=edit&type=' + encodeURIComponent(is.type)).then(r => { cache[ck] = r.status === 200 ? r.data : { tabs: [], rules: [] }; delete cache['pending:' + ck]; render(); });
        }
        return box;
      }
      const form = cache[ck], vals = Object.assign({ priority: is.priority, labels: is.labels || [], assignee: is.assignee && is.assignee.id }, is.custom || {});
      for (const tab of form.tabs) {
        const fs = tab.fields.filter(f => !f.builtin && !ruleState(form.rules, f, vals, form.role).hidden);
        if (!fs.length) continue;
        box.hidden = false;
        box.append(h('strong', {}, tab.name === 'Details' ? 'Fields' : tab.name));
        for (const f of fs) {
          const ctl = fieldControl(f, (is.custom || {})[f.key], !canEdit()); // the server enforces per-field edit roles
          const err = h('span', { class: 'err', role: 'alert' });
          ctl.el.addEventListener('change', async () => {
            const r2 = await api('PATCH', '/api/v1/issues/' + key, { custom: { [f.key]: ctl.get() } });
            if (r2.status === 200) { err.textContent = ''; await reloadAll(); } else err.textContent = (r2.data && r2.data.error) || 'Could not save';
          });
          box.append(h('div', {}, fieldRow(Object.assign({}, f, { required: ruleState(form.rules, f, vals, form.role).required }), ctl), err));
        }
      }
      return box;
    }

    // ----- description with autosaved drafts
    function descriptionEl(is) {
      const box = h('div', { class: 'card', style: 'padding:12px;margin:8px 0' });
      const head = h('div', { class: 'chips' }, h('strong', {}, 'Description'), noteFor('description', 'description'), h('span', { class: 'grow' }));
      if (canEdit()) head.append(h('button', { class: 'btn', onclick: () => openEditor() }, 'Edit'));
      box.append(head, is.description ? paragraphs(is.description) : h('p', { class: 'muted' }, 'No description.'));
      if (st.changedBy && !st.editing) box.append(h('div', { class: 'banner info' }, 'The description was changed by ' + st.changedBy + '.'));
      // an unsaved draft from an earlier session is offered back
      const ld = localDraft(key, 'description');
      if (ld && ld.body !== is.description && !st.editing && canEdit()) {
        box.append(h('div', { class: 'banner warn', role: 'status' }, 'Restored your unsaved draft from ' + L.relTime(ld.at) + '.',
          h('button', { class: 'btn', onclick: () => openEditor(ld) }, 'Continue editing'),
          h('button', { class: 'btn', onclick: () => { setLocalDraft(key, 'description', null); api('DELETE', '/api/v1/issues/' + key + '/drafts/description'); render(); } }, 'Discard')));
      }
      if (st.editing) return editorEl(box);
      return box;
    }
    async function openEditor(restored) {
      let ld = restored || localDraft(key, 'description');
      if (!ld) { // maybe a server draft from another device
        const r = await api('GET', '/api/v1/issues/' + key + '/drafts');
        const d = r.status === 200 && (r.data || []).find(x => x.field === 'description');
        if (d && d.body !== v.issue.description) ld = { body: d.body, base_seq: d.base_seq, at: d.updated_at };
      }
      st.editing = { body: ld ? ld.body : v.issue.description, baseSeq: ld ? ld.base_seq : v.issue.description_seq, restored: ld && ld.body !== v.issue.description ? ld : null };
      st.dirty = !!st.editing.restored; st.saveId = null;
      heartbeat('description');
      render();
    }
    function editorEl(box) {
      const ed = st.editing;
      clear(box);
      // A very large text is edited a window at a time (PERF-09): the browser lays out a text area of a megabyte so slowly that every key would stall the
      // page, so the area holds about WIN characters and the rest waits in head and tail; ed.body is always the whole text.
      const LARGE = 200000, WIN = 12000;
      let head = '', tail = '';
      const windowed = ed.body.length > LARGE;
      const writer = windowed ? draftWriter(key, 'description') : null;
      const ta = h('textarea', { 'aria-label': 'Description editor', value: windowed ? '' : ed.body });
      const bars = h('div', { class: 'chips', role: 'group', 'aria-label': 'Move through the text' });
      const kb = n => Math.max(1, Math.round(n / 1024)) + ' KB';
      function showWindow(full, at) {
        let s0 = Math.max(0, Math.min(at, full.length - WIN)), e0 = Math.min(full.length, s0 + WIN);
        if (s0 > 0) { const nl = full.indexOf('\n', s0); if (nl >= 0 && nl < e0) s0 = nl + 1; }
        if (e0 < full.length) { const nl = full.lastIndexOf('\n', e0); if (nl >= s0) e0 = nl + 1; }
        head = full.slice(0, s0); tail = full.slice(e0); ta.value = full.slice(s0, e0);
        if (writer) { writer.stage('h', head); writer.stage('t', tail); } // written in the background, a chunk at a time
        paintBars();
      }
      function paintBars() {
        clear(bars);
        bars.append(h('span', { class: 'muted' }, 'A large text (' + kb(head.length + ta.value.length + tail.length) + '): you are editing one part of it.'),
          h('button', { class: 'btn ghost', id: 'earlier-text', disabled: !head.length, onclick: () => shift(-1) }, head.length ? 'Earlier (' + kb(head.length) + ')' : 'Start of the text'),
          h('button', { class: 'btn ghost', id: 'later-text', disabled: !tail.length, onclick: () => shift(1) }, tail.length ? 'Later (' + kb(tail.length) + ')' : 'End of the text'));
      }
      function shift(dir) { const full = head + ta.value + tail; showWindow(full, head.length + (dir > 0 ? ta.value.length : -WIN)); }
      const setFull = text => { if (windowed) showWindow(text, 0); else ta.value = text; };
      if (windowed) showWindow(ed.body, 0);
      const state = h('span', { class: 'state', role: 'status' }, st.dirty ? 'Draft saved' : '');
      box.append(h('div', { class: 'chips' }, h('strong', {}, 'Description'), h('span', { class: 'grow' })), h('div', { id: 'editing-warning' }));
      if (ed.restored) box.append(h('div', { class: 'banner warn' }, 'Restored your unsaved draft from ' + L.relTime(ed.restored.at) + '.',
        h('button', { class: 'btn', onclick: () => { setFull(v.issue.description); ed.body = v.issue.description; ed.baseSeq = v.issue.description_seq; ed.restored = null; setLocalDraft(key, 'description', null); render(); } }, 'Discard')));
      box.append(h('div', { id: 'changed-banner' }));
      if (windowed) box.append(bars);
      box.append(ta);
      showChanged();
      ta.addEventListener('input', () => {
        ed.body = windowed ? head + ta.value + tail : ta.value; st.dirty = true; state.textContent = 'Editing…';
        // every 2 s while typing goes on, not 2 s after it stops: a timer restarted by each key would never fire for someone who does not pause (E3, SPEC 21)
        if (!st.localTimer) st.localTimer = setTimeout(flushLocal, 2000);
      });
      const flushLocal = () => {
        st.localTimer = 0;
        if (!st.editing || !st.dirty) return;
        if (windowed) writer.commit(ta.value, { base_seq: ed.baseSeq, at: new Date().toISOString() });
        else setLocalDraft(key, 'description', { body: ed.body, base_seq: ed.baseSeq, at: new Date().toISOString() });
        state.textContent = 'Draft saved';
      };
      // a tab that is hidden or going away writes what it has at once
      const leaving = () => { clearTimeout(st.localTimer); flushLocal(); };
      window.addEventListener('pagehide', leaving);
      document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'hidden') leaving(); });
      clearInterval(st.serverTimer);
      st.serverTimer = setInterval(() => { if (st.dirty && st.editing) api('PUT', '/api/v1/issues/' + key + '/drafts/description', { body: ed.body, base_seq: ed.baseSeq }); }, 10000);
      const save = async () => {
        if (st.saving) return; // one save at a time (a held Ctrl+Enter, a double click)
        st.saving = true;
        try { await saveOnce(); } finally { st.saving = false; }
      };
      ta.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); if (!e.repeat) save(); } });
      const saveOnce = async () => {
        clearTimeout(st.localTimer); st.localTimer = 0; setLocalDraft(key, 'description', { body: ed.body, base_seq: ed.baseSeq, at: new Date().toISOString() }); // never lose it
        state.textContent = 'Saving…';
        st.saveId = st.saveId || L.uuid();
        const r = await saveText('description', ed.body, ed.baseSeq, 'save', st.saveId);
        if (r.ok) { stopEditing(true); await reloadAll(); }
        else if (r.conflict) { st.saveId = null; await conflictDialog('description', r.conflict, ed.body, async () => { stopEditing(true); await reloadAll(); }); }
        else {
          state.textContent = r.msg || 'Offline, draft kept'; // same save id on retry: a replay can never apply twice
          if (r.paused) window.addEventListener('tl-resumed', () => { if (st.editing && st.dirty) save(); }, { once: true });
        }
      };
      box.append(h('div', { class: 'chips' }, state, h('span', { class: 'grow' }),
        h('button', { class: 'btn', onclick: () => { if (!st.dirty || confirm('Discard unsaved changes? Your draft is kept.')) stopEditing(false); render(); } }, 'Cancel'),
        h('button', { class: 'btn primary', onclick: save }, 'Save')));
      setTimeout(() => { ta.focus(); updatePresenceUI(); }, 0);
      return box;
    }
    // the banner is updated in place, so it can appear while the person is typing without touching their text
    function showChanged() {
      const bn = document.getElementById('changed-banner');
      if (!bn) return;
      clear(bn);
      if (st.changedBy) bn.append(h('div', { class: 'banner info', role: 'status' }, 'The description was changed by ' + st.changedBy + '. Nothing changes in your text until you save; saving will open the merge dialog.'));
    }
    function stopEditing(saved) {
      clearInterval(st.serverTimer); clearTimeout(st.localTimer); st.localTimer = 0;
      if (saved) { setLocalDraft(key, 'description', null); }
      st.editing = null; st.dirty = false; st.saveId = null; st.changedBy = null; heartbeat('');
    }

    // PUT text with compare-and-set; returns {ok} | {conflict} | {msg}
    async function saveText(field, body, baseSeq, mode, saveId) {
      let r;
      try { r = await api('PUT', '/api/v1/issues/' + key + '/text/' + field, { body, base_seq: baseSeq, mode, save_id: saveId || L.uuid() }); }
      catch (e) { return { msg: 'Offline, draft kept' }; }
      if (r.status === 200) return { ok: true, seq: r.data.seq, removed: r.data.removed };
      if (r.status === 409 && r.data.code === 'text_conflict') return { conflict: r.data.detail };
      if (r.status === 410) {
        st.deleted = st.deleted || {};
        toast('This issue was deleted; your text is kept in your draft.');
        render();
        return { msg: 'This issue was deleted; your text is kept in your draft' };
      }
      if (r.status === 503 && r.data && r.data.code === 'paused') return { msg: 'Paused for maintenance, draft kept', paused: true };
      if (r.status === 426) return { msg: 'Reload to continue, draft kept' };
      return { msg: (r.data && r.data.error) || 'Could not save' };
    }

    // ----- conflict dialog: theirs | merged (editable) | mine; repeats until the save is accepted
    async function conflictDialog(field, c, mine, done) {
      return new Promise(resolve => {
        const title = c.by + ' saved changes to the ' + field + ' while you were editing';
        c.merge.conflicts = c.merge.conflicts || []; // defensive: a missing list means no conflicts
        const choices = c.merge.conflicts.map(() => 'mine');
        const merged = h('textarea', { 'aria-label': 'Merged result', value: c.merge.merged });
        const recompute = () => { merged.value = L.applyChoices(c.merge.merged, c.merge.conflicts, choices); };
        const diffEl = (a, b) => h('div', { class: 'diff', tabindex: 0 }, L.blockDiff(a, b).map(d => h('span', { class: d.t }, (d.t === 'add' ? '+ ' : d.t === 'del' ? '− ' : '  ') + d.s)));
        const summary = c.merge.their_changes + ' change' + (c.merge.their_changes === 1 ? '' : 's') + ' by ' + c.by + ', ' + c.merge.your_changes + ' by you, ' + c.merge.conflicts.length + ' block' + (c.merge.conflicts.length === 1 ? '' : 's') + ' changed by both';
        const err = h('div', { class: 'err', role: 'alert' });
        const conflictsEl = h('div', {}, c.merge.conflicts.map((k, i) => h('div', { class: 'conflict', role: 'group', 'aria-label': 'Conflicting block ' + (i + 1) },
          h('div', {}, '⚠ Block ' + (k.block + 1) + ' changed by both'),
          h('div', { class: 'cols', style: 'margin:6px 0' },
            h('div', {}, h('div', { class: 'muted' }, 'Original'), h('div', { class: 'diff' }, h('span', { class: 'del' }, k.base || '(empty)'))),
            h('div', {}, h('div', { class: 'muted' }, 'Theirs'), h('div', { class: 'diff' }, k.theirs || '(removed)')),
            h('div', {}, h('div', { class: 'muted' }, 'Mine'), h('div', { class: 'diff' }, k.mine || '(removed)'))),
          h('div', { class: 'opts' }, ['mine', 'theirs'].map(opt => h('label', {}, h('input', { type: 'radio', name: 'c' + i, checked: opt === 'mine', onchange: () => { choices[i] = opt; recompute(); } }), ' Use ' + opt))))));
        const submit = async (body, mode) => {
          const r = await saveText(field, body, c.current_seq, mode); // always against the latest seq: you can never overwrite what you have not seen
          if (r.ok) { close(); resolve(); await done(); toast('Saved' + (mode === 'keep_mine' ? ' (their version stays in history)' : '')); }
          else if (r.conflict) { close(); resolve(); await conflictDialog(field, r.conflict, mine, done); }
          else err.textContent = r.msg;
        };
        const close = modal(h('div', {},
          h('h2', {}, title), h('div', { class: 'muted', role: 'status' }, summary + ' · ' + L.relTime(c.at)),
          h('div', { class: 'cols' },
            h('div', { class: 'col' }, h('h3', {}, 'Their changes'), diffEl(c.base, c.current)),
            h('div', { class: 'col' }, h('h3', {}, 'Merged result (editable)'), merged),
            h('div', { class: 'col' }, h('h3', {}, 'Your changes'), diffEl(c.base, mine))),
          conflictsEl, err,
          h('div', { class: 'actions' },
            h('button', { class: 'btn', onclick: async () => { try { await navigator.clipboard.writeText(mine); } catch (e) { /* clipboard blocked */ } if (field === 'description') stopEditing(false); close(); resolve(); await reloadAll(); } }, 'Copy mine and reload'),
            h('button', { class: 'btn', onclick: () => { if (field === 'description') stopEditing(false); setLocalDraft(key, field, { body: mine, base_seq: c.base_seq, at: new Date().toISOString() }); close(); resolve(); reloadAll(); } }, 'Keep theirs'),
            h('button', { class: 'btn', onclick: () => submit(mine, 'keep_mine') }, 'Keep mine'),
            h('button', { class: 'btn primary', onclick: () => submit(merged.value, 'merge') }, 'Save merged result'))));
        close.scrim.dataset.sticky = '1'; // a conflict is never dismissed by accident
      });
    }

    // ----- attachments
    function attachmentsEl() {
      const strip = h('div', { class: 'strip' });
      for (const a of v.issue ? v.attachments : []) {
        const chip = h('div', { class: 'chip' + (a.scan_state === 'blocked' ? ' blocked' : '') });
        if (a.thumb_state === 'ready') {
          const img = h('img', { alt: a.name, width: 40, height: 40 });
          api('POST', '/api/v1/attachments/' + a.id + '/link?thumb=1').then(r => { if (r.status === 200) img.src = r.data.url; });
          chip.append(img);
        }
        chip.append(h('div', {}, h('div', { class: 'nm' }, a.name), h('div', { class: 'muted' }, a.scan_state === 'blocked' ? 'Blocked: malware signature' : L.bytes(a.size) + (a.type_flag ? ' · type mismatch' : ''))));
        if (a.scan_state === 'clean') chip.append(h('button', { class: 'btn ghost', 'aria-label': 'Download ' + a.name, onclick: async () => { const r = await api('POST', '/api/v1/attachments/' + a.id + '/link'); if (r.status === 200) window.open(r.data.url, '_blank', 'noopener'); } }, '⬇'));
        if (v.request && canEdit() && !a.from_customer && a.scan_state === 'clean') chip.append(h('button', { class: 'btn ghost', 'aria-pressed': String(!!a.customer_visible), 'aria-label': (a.customer_visible ? 'Hide ' : 'Share ') + a.name + (a.customer_visible ? ' from the customer' : ' with the customer'), title: a.customer_visible ? 'Visible to the customer' : 'Internal: click to share with the customer', onclick: async () => { const r = await api('PUT', '/api/v1/attachments/' + a.id + '/customer-visible', { visible: !a.customer_visible }); if (r.status === 200) reloadAll(); else toast((r.data && r.data.error) || 'Could not change that'); } }, a.customer_visible ? '👁' : '🔒'));
        if (v.request && a.from_customer) chip.append(h('span', { class: 'muted', title: 'Sent by the customer' }, 'customer'));
        if (canEdit()) chip.append(h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + a.name, onclick: async () => { await api('DELETE', '/api/v1/issues/' + key + '/attachments/' + a.id); reloadAll(); } }, '×'));
        strip.append(chip);
      }
      const wrap = h('div', {}, strip);
      if (canEdit()) {
        const file = h('input', { type: 'file', multiple: true, class: 'sr', id: 'file' });
        file.addEventListener('change', () => uploadFiles([...file.files]));
        wrap.append(file, h('label', { for: 'file', class: 'btn' }, '📎 Attach'), h('span', { class: 'muted' }, ' or paste a screenshot / drop files'));
      }
      return wrap;
    }
    async function uploadFiles(files) {
      for (const f of files) {
        toast('Uploading ' + f.name + '…');
        if (await uploadOne(key, f)) await reloadAll();
      }
    }
    root.addEventListener('paste', e => { const fs = [...(e.clipboardData ? e.clipboardData.files : [])]; if (fs.length && canEdit()) { e.preventDefault(); uploadFiles(fs); } });
    root.addEventListener('dragover', e => e.preventDefault());
    root.addEventListener('drop', e => { if (e.dataTransfer && e.dataTransfer.files.length && canEdit()) { e.preventDefault(); uploadFiles([...e.dataTransfer.files]); } });

    // ----- development (Git): collapsed, filled from Tasklane's own store; the provider is never asked from here
    function developmentEl() {
      const body = h('div', { class: 'dev-body' });
      const sum = h('summary', {}, 'Development');
      const box = h('details', { class: 'dev', hidden: true }, sum, body);
      api('GET', '/api/v1/issues/' + key + '/development').then(r => {
        if (r.status !== 200 || !r.data.summary) return;
        box.hidden = false;
        sum.textContent = 'Development · ' + r.data.summary;
        const d = r.data;
        const group = (label, items, line) => items.length ? h('div', {}, h('strong', {}, label), h('ul', { class: 'dev-list' }, items.map(i => h('li', {}, line(i))))) : null;
        const link = (i, text) => i.url ? h('a', { href: i.url, rel: 'noopener noreferrer', target: '_blank' }, text) : text;
        body.append(
          group('Pull requests', d.pull_requests, i => [link(i, i.title), ' ', h('span', { class: 'loz ' + (i.state === 'merged' ? 'done' : 'todo') }, i.state + (i.review ? ', ' + i.review.replace('_', ' ') : '')), ' ', h('span', { class: 'muted' }, i.author)]),
          group('Branches', d.branches, i => link(i, i.title)),
          group('Commits', d.commits, i => [h('code', {}, i.sha.slice(0, 7)), ' ', link(i, i.title), ' ', h('span', { class: 'muted' }, i.author)]),
          group('Builds', d.builds, i => [i.title, ' ', h('span', { class: 'loz ' + (i.state === 'passing' ? 'done' : i.state === 'failing' ? 'blocked' : 'in_progress') }, i.state)]),
          group('Deployments', d.deployments, i => [i.environment, ' ', h('span', { class: 'muted' }, i.state)]));
      });
      return box;
    }

    // ----- comments and activity
    function threadEl() {
      const t = h('div', {});
      for (const c of v.comments) {
        t.append(h('div', { class: 'msg' }, avatar(c.author.name), h('div', { class: 'body' },
          h('div', {}, h('span', { class: 'who' }, c.author.name), ' ', h('span', { class: 'muted' }, L.relTime(c.created) + (c.internal ? ' · internal' : '') + (c.via_email ? ' · via email' : '') + (c.edited ? ' · edited' : ''))), paragraphs(c.body))));
      }
      if (!v.comments.length) t.append(h('p', { class: 'muted' }, 'No comments yet.'));
      return t;
    }
    // the whole history, newest first, on its own tab
    function historyEl() {
      const act = h('div', { class: 'history' }, h('p', { class: 'muted' }, 'Loading…'));
      api('GET', '/api/v1/issues/' + key + '/history').then(r => {
        if (r.status !== 200) { clear(act).append(h('p', { class: 'muted' }, 'The history could not be loaded.')); return; }
        clear(act);
        if (!r.data.length) act.append(h('p', { class: 'muted' }, 'No history yet.'));
        for (const x of [...r.data].reverse()) {
          const line = x.field === 'created' ? 'created this issue' : x.field === 'attachment' ? (x.after ? 'attached ' + x.after : 'removed ' + x.before) : 'changed ' + x.field + (x.before != null && x.after != null && x.field !== 'description' ? ' ' + x.before + ' → ' + x.after : '');
          act.append(h('div', { class: 'activity' }, '·· ' + x.actor + ' ' + line + ' · ' + L.relTime(x.at)));
        }
      });
      return act;
    }
    function replyEl() {
      if (!canEdit()) return h('div', { class: 'muted' }, 'You have read-only access to this project.');
      const ta = h('textarea', { placeholder: 'Reply…', 'aria-label': 'Reply' });
      const ld = localDraft(key, 'comment'); if (ld) ta.value = ld.body;
      let t; ta.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => setLocalDraft(key, 'comment', ta.value ? { body: ta.value, base_seq: 0, at: new Date().toISOString() } : null), 2000); });
      const internal = h('input', { type: 'checkbox', id: 'int' });
      // on a request raised through the portal, say who will read the reply before it is sent
      const audience = h('span', { class: 'muted', role: 'status', id: 'reply-audience' });
      const say = () => { audience.textContent = v.request ? (internal.checked ? 'Internal note: only staff see it.' : 'The customer (' + v.request.email + ') will see this reply in the portal.') : ''; };
      internal.addEventListener('change', say); say();
      // saved replies of the project, when there are any
      const canned = h('select', { 'aria-label': 'Insert a canned response', hidden: true }, h('option', { value: '' }, 'Canned response…'));
      api('GET', '/api/v1/projects/' + v.issue.project + '/canned-responses').then(r => {
        if (r.status !== 200 || !r.data.length) return;
        r.data.forEach(c => canned.append(h('option', { value: c.id }, c.name)));
        canned.hidden = false;
        canned.addEventListener('change', () => {
          const c = r.data.find(x => x.id === canned.value);
          canned.value = '';
          if (!c) return;
          const text = c.body.replace(/\{\{\s*issue\.key\s*\}\}/g, v.issue.key).replace(/\{\{\s*customer\s*\}\}/g, v.request ? (v.request.customer || v.request.email) : 'there').replace(/\{\{\s*agent\s*\}\}/g, (S.me && (S.me.label || S.me.name)) || '');
          ta.value = ta.value ? ta.value + '\n' + text : text;
          ta.dispatchEvent(new Event('input'));
          ta.focus();
        });
      });
      // one request at a time, so a held Ctrl+Enter or a double click never sends the comment twice (KEY-08)
      const send = h('button', { class: 'btn primary', onclick: async () => {
        if (!ta.value.trim() || send.disabled) return;
        send.disabled = true;
        try {
          const r = await api('POST', '/api/v1/issues/' + key + '/comments', { body: ta.value, internal: internal.checked });
          if (r.status === 201) { setLocalDraft(key, 'comment', null); await reloadAll(); } else toast(r.data.error || 'Could not send');
        } finally { send.disabled = false; }
      } }, 'Send');
      ta.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); if (!e.repeat) send.click(); } }); // Enter alone is a new line
      return h('div', { class: 'reply' }, ta, h('div', { class: 'chips' }, h('label', {}, internal, ' Internal'), canned, audience, h('span', { class: 'grow' }),
        send));
    }

    // ----- presence heartbeat and change polling (advisory; never blocks anything)
    function heartbeat(field) {
      st.lastField = field;
      if (rtSend({ op: 'presence', channel: 'issue:' + key, field })) return; // over the socket: state changes only
      api('POST', '/api/v1/issues/' + key + '/presence', { field }).then(r => { if (r.status === 200 && Array.isArray(r.data)) { v.presence = r.data; updatePresenceUI(); } }).catch(() => {});
    }
    let etag = null;
    async function poll() {
      if (document.hidden) { st.stale = true; return; } // refetched when the tab is shown again (events are best effort)
      let r; try { r = await fetch('/api/v1/issues/' + key + '/view', { credentials: 'same-origin', headers: etag ? { 'If-None-Match': etag } : {} }); } catch (e) { return; }
      if (r.status === 304 || r.status !== 200) return;
      etag = r.headers.get('ETag'); const nv = await r.json();
      const before = v.issue;
      if (st.editing) { // never touch the editor's text; only warn
        if (nv.issue.description_seq !== st.editing.baseSeq && nv.issue.description_seq !== before.description_seq) {
          const last = (await api('GET', '/api/v1/issues/' + key + '/history')).data || [];
          const e = [...last].reverse().find(x => x.field === 'description');
          st.changedBy = e ? e.actor : 'someone else';
        }
        showChanged();
        v.presence = nv.presence; v.issue = Object.assign({}, nv.issue, { description: before.description, description_seq: before.description_seq });
        const ed = document.activeElement; if (!ed || ed.tagName !== 'TEXTAREA') render(); return;
      }
      const changed = nv.issue.version !== before.version || nv.issue.description_seq !== before.description_seq || nv.issue.summary_seq !== before.summary_seq || nv.comments.length !== v.comments.length || nv.attachments.length !== v.attachments.length;
      const typing = document.activeElement && ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName);
      v = nv;
      if (!typing) { render(); if (changed) root.classList.add('flash'); setTimeout(() => root.classList.remove('flash'), 1300); }
    }
    async function reloadAll() { const nv = await load(); if (nv) { v = nv; st.v = nv; render(); } }
    S.timers.push(setInterval(() => { if (!st.editing) heartbeat(''); else heartbeat('description'); }, 10000));
    const onVisible = () => { if (!document.hidden && st.stale) { st.stale = false; poll(); } };
    document.addEventListener('visibilitychange', onVisible);
    let tick = 0;
    S.timers.push(setInterval(() => { if (!RT.up || ++tick % 8 === 0) poll(); }, 4000)); // fallback: every 4 s without the socket, every 32 s with it
    rtSub(key, m => {
      if (m.type === 'presence') { v.presence = m.presence; updatePresenceUI(); }
      else if (m.type === 'event' && m.event.type === 'issue.deleted') { st.deleted = { by: m.event.actor }; render(); }
      else if (m.type === 'event' && m.event.type === 'issue.moved') { const nk = (m.event.fields || [])[0]; if (nk) { toast(m.event.actor + ' moved this issue to ' + nk); location.replace('#/issue/' + nk); } }
      else if (m.type === 'event') { if (m.event.actor_id !== S.me.id || m.event.type === 'attachment.changed') poll(); }
      else if (m.type === 'error' && m.code === 'denied') toast('You no longer have access to this issue.');
    });
    S.cleanup = () => { rtUnsub(key); document.removeEventListener('visibilitychange', onVisible); S.cleanup = null; };
    heartbeat('');
    window.onbeforeunload = e => { if (st.dirty) { e.preventDefault(); e.returnValue = ''; } };
  }

  // ---------- trash ----------
  async function trashView(projectKey) {
    stopTimers();
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === projectKey }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/trash/' + sel.value; });
    const box = h('div', { class: 'card', role: 'list' });
    clear($app).append(topbar(), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Trash'), sel), h('p', { class: 'muted' }, 'Deleted issues can be restored until their retention ends; their keys stay reserved.'), box));
    const r = await api('GET', '/api/v1/projects/' + projectKey + '/trash');
    if (r.status !== 200) { box.append(h('div', { class: 'row' }, 'The trash could not be loaded.')); return; }
    if (!r.data.length) box.append(h('div', { class: 'row' }, h('span', { class: 'muted' }, 'The trash is empty.')));
    for (const e of r.data) {
      box.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'key' }, e.key), h('span', { class: 'sum' }, e.summary),
        h('span', { class: 'muted' }, 'deleted by ' + e.deleted_by + ' · purged ' + new Date(e.purge_after).toISOString().slice(0, 10) + (e.legal_hold ? ' · legal hold' : '')),
        h('button', { class: 'btn', onclick: async () => {
          const rr = await api('POST', '/api/v1/issues/' + e.key + '/lifecycle', { op: 'restore' });
          if (rr.status === 200) { toast('Restored ' + e.key); trashView(projectKey); } else toast((rr.data && rr.data.error) || 'Could not restore');
        } }, 'Restore')));
    }
  }

  // ---------- routing ----------
  function route() {
    setTimeout(bell, 0);
    window.onbeforeunload = null;
    if (!S.me) return loginView();
    const hash = location.hash || '#/';
    let m;
    if ((m = hash.match(/^#\/trash\/([A-Za-z0-9]*)/))) return trashView(m[1].toUpperCase() || (S.projects[0] && S.projects[0].key) || '');
    if ((m = hash.match(/^#\/issue\/([A-Za-z0-9-]+)/))) return issueView(m[1].toUpperCase());
    for (const x of EXT.routes) { const mm = hash.match(x.re); if (mm) { stopTimers(); return x.run(mm); } }
    // #/?q=…&syntax=text|tql; a link without syntax (older links, Releases' "Issues") is TQL
    const p = new URLSearchParams(hash.startsWith('#/?') ? hash.slice(3) : '');
    if (p.has('q')) S.syntax = p.get('syntax') === 'text' ? 'text' : 'tql';
    return listView(p.get('q') || '');
  }
  // the unread count on the Inbox button: fetched on every screen and every 30 seconds
  async function bell() {
    if (!S.me) return;
    const r = await api('GET', '/api/v1/notifications/unread-count');
    const b = document.getElementById('bell-n'), btn = document.getElementById('bell');
    if (!b || r.status !== 200) return;
    b.hidden = !r.data.unread; b.textContent = r.data.unread > 99 ? '99+' : String(r.data.unread);
    if (btn) btn.setAttribute('aria-label', r.data.unread ? 'Inbox, ' + r.data.unread + ' unread' : 'Inbox');
  }
  setInterval(bell, 30000);
  // scheduled maintenance announced by the operator of a hosted installation, shown until it is over
  async function showScheduled() {
    try {
      const r = await api('GET', '/api/v1/system/status');
      const list = (r.status === 200 && r.data && r.data.scheduled) || [];
      if (!list.length || MAINT.on) return;
      const n = list[0], f = d => new Date(d).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
      banner('info', 'Scheduled maintenance: ' + f(n.starts_at) + ' to ' + f(n.ends_at) + (n.message ? '. ' + n.message : '') + ' Reading continues; changes made during it are kept as drafts.');
    } catch (e) { /* the banner is a courtesy */ }
  }
  async function boot() {
    const r = await api('GET', '/api/v1/bootstrap');
    if (r.status === 200) {
      S.me = r.data.me; S.projects = r.data.projects; S.users = r.data.users; rtConnect();
      loadPrefs(r.data.ui_prefs); // kept on the server, so they follow the person to every browser
    }
    else if (r.status === 403 && r.data && r.data.code === 'second_factor_required') {
      // signed in, but held to the pages that set a second factor up: who they are is all the page may load before that
      const m = await api('GET', '/api/v1/me');
      if (m.status === 200) { S.me = m.data; S.projects = []; S.users = []; } else S.me = null;
    } else { S.me = null; if (r.status === 401) forgetPrefs(); }
    applyWidth();
    route();
    if (S.me) showScheduled();
  }
  // the agile views (agile.js) plug in here
  const EXT = { routes: [], settings: null };
  window.TLApp = { b64u, bell, h, clear, api, toast, topbar, modal, S, EXT, avatar, lozenge, PRI, timer: (fn, ms) => S.timers.push(setInterval(fn, ms)), route, P, saveSettings, PREF_DEFAULTS, createDialog, projectDialog, undoToast, WANT_UNDO };
  window.addEventListener('hashchange', route);
  boot();
})();
