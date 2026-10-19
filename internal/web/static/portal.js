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
  // append that skips nothing-children: Element.append(null) would write the word "null"
  const add = (el, ...kids) => { for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) el.append(k); return el; };
  const errText = r => (r.data && r.data.error) || 'Something went wrong';


  // The sign-in check: find a counter that makes SHA-256 of "challenge:counter" begin with enough zero bits. A small synchronous SHA-256 (the
  // browser's own is asynchronous and far too slow for this); the work runs in slices so the page stays responsive.
  const K = new Uint32Array([0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2]);
  function sha256(bytes) {
    const n = bytes.length, total = ((n + 9 + 63) >> 6) << 6, buf = new Uint8Array(total);
    buf.set(bytes); buf[n] = 0x80;
    const dv = new DataView(buf.buffer); dv.setUint32(total - 4, n * 8 >>> 0); dv.setUint32(total - 8, Math.floor(n / 0x20000000));
    const H = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]), w = new Uint32Array(64);
    const rr = (x, k) => (x >>> k) | (x << (32 - k));
    for (let o = 0; o < total; o += 64) {
      for (let i = 0; i < 16; i++) w[i] = dv.getUint32(o + i * 4);
      for (let i = 16; i < 64; i++) { const a = w[i - 15], b = w[i - 2]; w[i] = (w[i - 16] + (rr(a, 7) ^ rr(a, 18) ^ (a >>> 3)) + w[i - 7] + (rr(b, 17) ^ rr(b, 19) ^ (b >>> 10))) >>> 0; }
      let [a, b, c, d, e, f, g, hh] = H;
      for (let i = 0; i < 64; i++) {
        const t1 = (hh + (rr(e, 6) ^ rr(e, 11) ^ rr(e, 25)) + ((e & f) ^ (~e & g)) + K[i] + w[i]) >>> 0, t2 = ((rr(a, 2) ^ rr(a, 13) ^ rr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0;
        hh = g; g = f; f = e; e = (d + t1) >>> 0; d = c; c = b; b = a; a = (t1 + t2) >>> 0;
      }
      H[0] += a; H[1] += b; H[2] += c; H[3] += d; H[4] += e; H[5] += f; H[6] += g; H[7] += hh;
    }
    return H;
  }
  const zeroBits = H => { let z = 0; for (let i = 0; i < 8; i++) { if (H[i] === 0) { z += 32; continue; } return z + Math.clz32(H[i]); } return z; };
  async function solveCheck(ch) {
    const enc = new TextEncoder(), prefix = ch.challenge + ':';
    for (let n = 0; ; ) {
      const stop = n + 20000;
      for (; n < stop; n++) if (zeroBits(sha256(enc.encode(prefix + n))) >= ch.bits) return String(n);
      await new Promise(r => setTimeout(r, 0));
    }
  }

  async function start() {
    if (!key) { add(clear(root), h('h1', {}, 'Support portal'), h('p', {}, 'Open the address of the portal you were given.')); return; }
    const info = await api('GET', '/api/v1/portal/' + key);
    if (info.status !== 200) { add(clear(root), h('h1', {}, 'Support portal'), h('p', {}, 'This portal does not exist or is not open.')); return; }
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
    return signIn(info.data, new URLSearchParams(location.search).get('sso') === 'denied' ? 'That sign-in was not accepted. Use another way below, or ask your administrator.' : '');
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
      let check;
      if (info.check) { // the portal asks for a proof of work first
        msg.className = 'muted'; msg.setAttribute('role', 'status'); msg.textContent = 'Checking your browser…';
        const c = await api('GET', '/api/v1/portal/' + key + '/challenge');
        if (c.status === 200 && c.data.required) check = { challenge: c.data.challenge, solution: await solveCheck(c.data) };
        msg.setAttribute('role', 'alert'); msg.textContent = '';
      }
      const r = await api('POST', '/api/v1/portal/' + key + '/code', { email: email.value, website: trap.value, check });
      if (r.status === 200) { codeRow.hidden = false; code.focus(); msg.className = 'muted'; msg.textContent = 'If that address can use this portal, a code is on its way.'; }
      else { msg.className = 'err'; msg.textContent = r.status === 429 ? 'Too many tries. Please wait a while and try again.' : errText(r); }
    });
    const sso = info.sso ? h('p', {}, h('a', { class: 'btn primary', id: 'psso', href: '/api/v1/auth/sso/' + encodeURIComponent(info.sso.name) + '/start?redirect=' + encodeURIComponent('/portal/' + key) }, 'Sign in with ' + info.sso.label), h('span', { class: 'muted' }, ' or with an emailed code:')) : null;
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
    add(clear(root), h('h1', {}, info.name), info.welcome ? h('p', {}, info.welcome) : null, h('h2', {}, 'Sign in'), sso, form, notices(info));
  }

  function notices(info) {
    return h('footer', { class: 'muted' }, info.consent_text ? h('p', {}, info.consent_text) : null, info.privacy_notice ? h('details', {}, h('summary', {}, 'Privacy notice'), h('p', { style: 'white-space:pre-wrap' }, info.privacy_notice)) : null);
  }

  // ---- signed in: requests, a new request, details ----
  function home(info, me) {
    const nav = (href, label, current) => h('a', { href, class: 'tab', 'aria-current': current ? 'page' : null }, label);
    function frame(current, ...body) {
      add(clear(root), 
        h('h1', {}, info.name),
        h('p', {}, 'Signed in as ', h('strong', {}, me.email), '. ', h('button', { class: 'btn ghost', onclick: async () => { await api('POST', '/api/v1/portal/logout'); location.hash = ''; location.reload(); } }, 'Sign out')),
        h('nav', { class: 'chips', 'aria-label': 'Portal' }, nav('#/', 'My requests', current === 'list'), nav('#/new', 'New request', current === 'new'), nav('#/me', 'My details', current === 'me')),
        ...body, notices(info));
    }
    const route = () => {
      const m = location.hash.match(/^#\/r\/([A-Za-z0-9-]+)$/);
      if (m) return detail(m[1]);
      if (location.hash === '#/new') return newRequest();
      if (location.hash === '#/me') return details();
      return list();
    };

    async function list() {
      const ul = h('ul', { class: 'list', 'aria-label': 'Your requests' });
      frame('list', h('h2', {}, 'My requests'), ul);
      const r = await api('GET', '/api/v1/portal/' + key + '/requests');
      if (r.status !== 200) { add(ul, h('li', {}, errText(r))); return; }
      if (!r.data.length) add(ul, h('li', { class: 'muted' }, 'You have no requests yet. ', h('a', { href: '#/new' }, 'Make one.')));
      for (const q of r.data) add(ul, h('li', { class: 'row' }, h('a', { href: '#/r/' + q.key }, q.key + ' ' + q.summary), h('span', { class: 'muted' }, ' ' + q.status + (q.request_type ? ' · ' + q.request_type : '') + ' · ' + new Date(q.updated).toLocaleDateString())));
    }

    async function newRequest() {
      const box = h('div', {});
      frame('new', h('h2', {}, 'New request'), box);
      const r = await api('GET', '/api/v1/portal/' + key + '/request-types');
      if (r.status !== 200) { add(box, h('p', { class: 'err' }, errText(r))); return; }
      if (!r.data.length) { add(box, h('p', {}, 'There is nothing to choose from yet.')); return; }
      const sel = h('select', { id: 'rt', 'aria-label': 'What do you need help with?' }, r.data.map(t => h('option', { value: t.id }, t.name)));
      const form = h('form', {});
      const msg = h('p', { role: 'alert', class: 'err' });
      function draw() {
        const t = r.data.find(x => x.id === sel.value);
        clear(form);
        const inputs = {};
        const summary = h('input', { type: 'text', id: 'rs', maxlength: 255, required: true });
        const desc = h('textarea', { id: 'rd', rows: 6, maxlength: 20000 });
        const trap = h('input', { type: 'text', name: 'website', tabindex: '-1', autocomplete: 'off', 'aria-hidden': 'true', style: 'position:absolute;left:-9999px;width:1px;height:1px' });
        add(form, t.description ? h('p', { class: 'muted' }, t.description) : null, t.help_text ? h('p', {}, t.help_text) : null,
          h('label', { for: 'rs' }, 'Summary'), summary, h('label', { for: 'rd' }, 'Details'), desc);
        for (const f of t.fields) {
          const id = 'f-' + f.key;
          let el;
          if (f.type === 'textarea') el = h('textarea', { id, rows: 3 });
          else if ((f.type === 'select' || f.type === 'radio') && f.options) el = h('select', { id }, h('option', { value: '' }, ''), (f.options || []).map(o => h('option', { value: o }, o)));
          else if (f.type === 'checkbox') el = h('input', { type: 'checkbox', id });
          else el = h('input', { type: f.type === 'number' ? 'number' : f.type === 'date' ? 'date' : f.type === 'url' ? 'url' : 'text', id });
          if (f.required) el.setAttribute('required', '');
          inputs[f.key] = [el, f];
          add(form, h('label', { for: id }, f.label + (f.required ? ' (required)' : '')), f.help ? h('div', { class: 'muted' }, f.help) : null, el);
        }
        add(form, trap, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Send request')), msg);
        form.onsubmit = async e => {
          e.preventDefault(); msg.textContent = '';
          const fields = {};
          for (const k in inputs) { const [el, f] = inputs[k]; const v = f.type === 'checkbox' ? el.checked : el.value; if (v !== '' && v !== false) fields[k] = f.type === 'number' ? Number(v) : v; }
          const x = await api('POST', '/api/v1/portal/' + key + '/requests', { request_type: t.id, summary: summary.value, description: desc.value, fields, website: trap.value });
          if (x.status === 201) location.hash = x.data.key ? '#/r/' + x.data.key : '#/';
          else msg.textContent = x.status === 429 ? 'You have sent a lot of requests. Please wait a while.' : errText(x);
        };
      }
      sel.addEventListener('change', draw);
      add(box, h('label', { for: 'rt' }, 'What do you need help with?'), sel, form);
      draw();
    }

    async function detail(k) {
      const body = h('div', {});
      frame('list', body);
      const r = await api('GET', '/api/v1/portal/' + key + '/requests/' + encodeURIComponent(k));
      if (r.status !== 200) { add(body, h('p', {}, 'That request does not exist.'), h('a', { href: '#/' }, 'Back to my requests')); return; }
      const q = r.data;
      const thread = h('ul', { class: 'list', 'aria-label': 'Conversation' }, (q.comments || []).map(c => h('li', { class: 'row' }, h('strong', {}, c.author), h('span', { class: 'muted' }, ' ' + new Date(c.created).toLocaleString()), h('div', { style: 'white-space:pre-wrap' }, c.body))));
      const text = h('textarea', { id: 'cb', rows: 3, maxlength: 10000 });
      const msg = h('p', { role: 'alert', class: 'err' });
      add(body, h('h2', {}, q.key + ' ' + q.summary), h('p', {}, h('strong', {}, q.status), q.request_type ? ' · ' + q.request_type : '', ' · opened ' + new Date(q.created).toLocaleDateString(), q.raised_by ? ' · raised by ' + q.raised_by : ''),
        q.description ? h('p', { style: 'white-space:pre-wrap' }, q.description) : null,
        q.fields && q.fields.length ? h('dl', {}, q.fields.map(f => [h('dt', {}, f.label), h('dd', {}, f.value)]).flat()) : null,
        h('h3', {}, 'Conversation'), (q.comments || []).length ? thread : h('p', { class: 'muted' }, 'No replies yet.'),
        h('form', { onsubmit: async e => {
          e.preventDefault(); msg.textContent = '';
          const x = await api('POST', '/api/v1/portal/' + key + '/requests/' + encodeURIComponent(k) + '/comments', { body: text.value });
          if (x.status === 201) detail(k); else msg.textContent = x.status === 429 ? 'Please wait a while before writing more.' : errText(x);
        } }, h('label', { for: 'cb' }, 'Add a reply'), text, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Send')), msg),
        filesEl(q, k),
        shareEl(q, k),
        h('p', {}, h('a', { href: '#/' }, 'Back to my requests')));
    }

    // files on a request: the ones shared with this customer, and (when the portal takes them) a way to add more
    function filesEl(q, k) {
      const rules = info && info.files;
      const list = q.attachments || [];
      if (!rules && !list.length) return null;
      const msg = h('p', { role: 'alert', class: 'err' });
      const input = h('input', { type: 'file', id: 'pf', 'aria-describedby': 'pf-help' });
      const send = async () => {
        msg.textContent = '';
        const f = input.files[0];
        if (!f) { msg.textContent = 'Choose a file first.'; return; }
        const fd = new FormData(); fd.append('file', f);
        const r = await fetch('/api/v1/portal/' + key + '/requests/' + encodeURIComponent(k) + '/attachments', { method: 'POST', body: fd, credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } });
        if (r.status === 201) { detail(k); return; }
        let e = {}; try { e = await r.json(); } catch (x) { /* none */ }
        msg.textContent = r.status === 429 ? 'Please wait a while before sending more files.' : (e.error || 'The file could not be added.');
      };
      return h('div', {}, h('h3', {}, 'Files'),
        list.length ? h('ul', { class: 'list', 'aria-label': 'Files' }, list.map(a => h('li', { class: 'row' },
          h('a', { href: '/api/v1/portal/' + key + '/attachments/' + a.id, download: a.name }, a.name), h('span', { class: 'muted' }, ' ' + Math.max(1, Math.round(a.size / 1024)) + ' KB' + (a.mine ? ' · yours' : '')),
          a.mine ? h('button', { class: 'btn ghost', 'aria-label': 'Remove ' + a.name, onclick: async () => { const x = await api('DELETE', '/api/v1/portal/' + key + '/attachments/' + a.id); if (x.status === 200) detail(k); } }, 'Remove') : null))) : h('p', { class: 'muted' }, 'No files yet.'),
        rules && !q.resolved ? h('div', {}, h('label', { for: 'pf' }, 'Add a file'), input,
          h('p', { id: 'pf-help', class: 'muted' }, 'Up to ' + rules.max_mb + ' MB each, ' + rules.max_files + ' files per request. Accepted: ' + (rules.classes || []).map(c => ({ image: 'images', document: 'documents', log: 'text and logs' }[c] || c)).join(', ') + '.'),
          h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: send }, 'Upload')), msg) : null);
    }

    // who else sees this request: only the person who raised it can change that
    function shareEl(q, k) {
      if (!q.mine || !q.sharing) return null;
      const sh = q.sharing, msg = h('p', { role: 'alert', class: 'err' });
      const send = async body => { msg.textContent = ''; const x = await api('POST', '/api/v1/portal/' + key + '/requests/' + encodeURIComponent(k) + '/share', body); if (x.status === 200) detail(k); else msg.textContent = errText(x); };
      const org = sh.can_share_with_organization ? h('label', {}, h('input', { type: 'checkbox', id: 'sh-org', checked: sh.shared_with_organization, onchange: e => send({ organization: e.target.checked }) }), ' Everyone at ' + sh.organization + ' can see this request') : null;
      const who = h('input', { type: 'email', id: 'sh-add', maxlength: 254, autocomplete: 'off' });
      const people = h('ul', { class: 'list', 'aria-label': 'People who can also see this request' }, (sh.people || []).map(p => h('li', { class: 'row' }, (p.name ? p.name + ' · ' : '') + p.email, h('button', { class: 'btn ghost', type: 'button', 'aria-label': 'Stop sharing with ' + p.email, onclick: () => send({ remove: [p.email] }) }, 'Remove'))));
      return h('section', { 'aria-label': 'Sharing' }, h('h3', {}, 'Who can see this'), h('p', { class: 'muted' }, 'Only you, unless you share it. People you share it with see this request and its conversation, nothing else of yours.'), org,
        (sh.people || []).length ? people : null,
        h('form', { onsubmit: e => { e.preventDefault(); if (who.value.trim()) send({ add: [who.value.trim()] }); } }, h('label', { for: 'sh-add' }, 'Share with someone by email address'), who, h('button', { class: 'btn', type: 'submit' }, 'Share')), msg);
    }

    function details() {
      const name = h('input', { type: 'text', id: 'pn', value: me.name || '', maxlength: 100 });
      const tz = h('input', { type: 'text', id: 'pz', value: me.timezone || '', placeholder: 'Europe/Prague' });
      const msg = h('p', { role: 'status' });
      const pass = h('input', { type: 'password', id: 'pnp', autocomplete: 'new-password' });
      frame('me',
        h('h2', {}, 'Your details'),
        h('form', { onsubmit: async e => {
          e.preventDefault();
          const r = await api('PUT', '/api/v1/portal/me', { name: name.value, language: me.language || '', timezone: tz.value });
          msg.className = r.status === 200 ? 'muted' : 'err'; msg.textContent = r.status === 200 ? 'Saved.' : errText(r);
        } }, h('label', { for: 'pn' }, 'Name'), name, h('label', { for: 'pz' }, 'Time zone'), tz, h('button', { class: 'btn primary', type: 'submit' }, 'Save')),
        h('details', {}, h('summary', {}, me.has_password ? 'Change or remove my password' : 'Set a password'),
          h('label', { for: 'pnp' }, 'New password (leave empty to remove it)'), pass,
          h('button', { class: 'btn', type: 'button', onclick: async () => { const r = await api('PUT', '/api/v1/portal/password', { password: pass.value }); msg.className = r.status === 200 ? 'muted' : 'err'; msg.textContent = r.status === 200 ? 'Password saved.' : errText(r); } }, 'Save password')),
        h('h2', {}, 'Your data'),
        h('p', {}, h('button', { class: 'btn', type: 'button', id: 'dl', onclick: async () => {
          const r = await fetch('/api/v1/portal/me/export', { credentials: 'same-origin' });
          if (!r.ok) { msg.className = 'err'; msg.textContent = 'Could not prepare your data.'; return; }
          const url = URL.createObjectURL(await r.blob());
          const a = h('a', { href: url, download: 'my-data.json' }); document.body.append(a); a.click(); a.remove(); URL.revokeObjectURL(url);
        } }, 'Download my data')),
        eraseBox(me, msg),
        msg);
    }
    // erasing: the person types their address, and may also have what they wrote deleted
    function eraseBox(me, msg) {
      const confirm = h('input', { type: 'email', id: 'ec', autocomplete: 'off' });
      const content = h('input', { type: 'checkbox', id: 'ed' });
      return h('details', {}, h('summary', {}, 'Delete my data'),
        h('p', {}, 'This removes you from our records: your details, sign-ins and the emails we hold. Your requests stay with us as our work records, without your name or address.'),
        h('label', { for: 'ed' }, content, ' Also delete what I wrote (my comments, and the summary and details of my requests)'),
        h('label', { for: 'ec' }, 'Type your email address to confirm'), confirm,
        h('button', { class: 'btn', type: 'button', id: 'erase', onclick: async () => {
          const r = await api('POST', '/api/v1/portal/me/erase', { confirm: confirm.value, delete_content: content.checked });
          if (r.status === 200) { location.hash = ''; location.reload(); } else { msg.className = 'err'; msg.textContent = errText(r); }
        } }, 'Delete my data'));
    }
    window.addEventListener('hashchange', route);
    route();
  }
  start();
})();
