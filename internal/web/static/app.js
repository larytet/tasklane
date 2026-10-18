(function () {
  'use strict';
  const L = window.TL;
  const $app = document.getElementById('app');
  const S = { me: null, projects: [], users: [], q: '', view: null, timers: [] };

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
  async function api(method, path, body, headers) {
    const r = await fetch(path, { method, credentials: 'same-origin', headers: Object.assign({ 'X-Requested-With': 'tasklane', 'Content-Type': 'application/json' }, headers || {}), body: body === undefined ? undefined : JSON.stringify(body) });
    let data = null; try { data = await r.json(); } catch (e) { /* empty body, 304 */ }
    return { status: r.status, data, headers: r.headers };
  }
  function toast(msg, action) {
    const t = h('div', { class: 'toast' }, h('span', {}, msg));
    if (action) t.append(h('button', { class: 'btn', onclick: () => { action.fn(); t.remove(); } }, action.label));
    t.append(h('button', { class: 'btn ghost', 'aria-label': 'Dismiss', onclick: () => t.remove() }, '×'));
    document.getElementById('toasts').append(t);
    setTimeout(() => t.remove(), 12000);
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
  const RT = { ws: null, up: false, subs: new Map(), backoff: 1000, timer: 0 };
  async function rtConnect() {
    clearTimeout(RT.timer);
    if (!S.me || RT.ws) return;
    let tk;
    try { const r = await api('POST', '/api/v1/ws/ticket'); if (r.status !== 200) throw new Error('no ticket'); tk = r.data.ticket; } catch (e) { return rtRetry(); }
    const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/v1/ws?ticket=' + encodeURIComponent(tk));
    RT.ws = ws;
    ws.onopen = () => { RT.up = true; RT.backoff = 1000; for (const k of RT.subs.keys()) ws.send(JSON.stringify({ op: 'sub', channel: 'issue:' + k })); };
    ws.onmessage = ev => { let m; try { m = JSON.parse(ev.data); } catch (e) { return; } const k = m.issue || (m.event && m.event.issue) || (m.channel || '').replace('issue:', ''); const h = RT.subs.get(k); if (h) h(m); };
    ws.onclose = () => { RT.up = false; RT.ws = null; rtRetry(); };
    ws.onerror = () => { try { ws.close(); } catch (e) { /* already closed */ } };
  }
  function rtRetry() { if (!S.me) return; RT.timer = setTimeout(rtConnect, RT.backoff); RT.backoff = Math.min(RT.backoff * 2, 30000); }
  function rtSub(key, handler) { RT.subs.set(key, handler); if (RT.up) RT.ws.send(JSON.stringify({ op: 'sub', channel: 'issue:' + key })); else rtConnect(); }
  function rtUnsub(key) { RT.subs.delete(key); if (RT.up) RT.ws.send(JSON.stringify({ op: 'unsub', channel: 'issue:' + key })); }
  function rtSend(m) { if (RT.up) { RT.ws.send(JSON.stringify(m)); return true; } return false; }

  // ---------- shell ----------
  function topbar() {
    const q = h('input', { type: 'search', placeholder: 'Search or TQL…  e.g. project = ABC AND status = "To Do"', class: 'grow', 'aria-label': 'Search or TQL', value: S.q });
    q.addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      S.q = q.value;
      const target = '#/?q=' + encodeURIComponent(q.value);
      if (location.hash === target) route(); // the same search again is a refresh: no hashchange fires for an identical hash
      else location.hash = target;
    });
    return h('div', { class: 'bar' },
      h('span', { class: 'brand', tabindex: 0, role: 'link', onclick: () => { location.hash = '#/'; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/'; } }, 'Tasklane'),
      q,
      h('button', { class: 'btn primary', onclick: createDialog }, '+ Create'),
      h('button', { class: 'btn ghost', onclick: () => { location.hash = '#/boards/' + (S.projects[0] ? S.projects[0].key : ''); } }, 'Boards'),
      h('button', { class: 'btn ghost', onclick: () => { location.hash = '#/trash/' + (S.projects[0] ? S.projects[0].key : ''); } }, 'Trash'),
      h('button', { class: 'btn ghost', 'aria-label': 'Toggle theme', onclick: () => theme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark') }, '◐'),
      h('span', { class: 'muted' }, S.me ? S.me.label : ''),
      h('button', { class: 'btn ghost', onclick: async () => { await api('POST', '/api/v1/logout'); S.me = null; route(); } }, 'Sign out'));
  }

  function loginView() {
    const email = h('input', { type: 'email', id: 'email', autocomplete: 'username' }), pw = h('input', { type: 'password', id: 'pw', autocomplete: 'current-password' });
    const msg = h('div', { class: 'err', role: 'alert' });
    const go = async e => {
      e.preventDefault();
      const r = await api('POST', '/api/v1/login', { email: email.value, password: pw.value });
      if (r.status === 200) { await boot(); } else msg.textContent = (r.data && r.data.error) || 'Sign in failed';
    };
    clear($app).append(h('form', { class: 'login card', onsubmit: go },
      h('h1', {}, 'Tasklane'), h('label', { for: 'email' }, 'Email'), email, h('label', { for: 'pw' }, 'Password'), pw, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Sign in'))));
    email.focus();
  }

  // ---------- issue list ----------
  async function listView(q) {
    stopTimers();
    S.q = q || '';
    const out = h('div', { class: 'card', role: 'list' }), err = h('div', { class: 'tqlerr', role: 'alert' }), meta = h('div', { class: 'muted' });
    clear($app).append(topbar(), h('div', { class: 'page' }, err, meta, out));
    const r = await api('GET', '/api/v1/issues?limit=100&q=' + encodeURIComponent(S.q));
    if (r.status === 400) { err.textContent = r.data.error + (r.data.detail && r.data.detail.pos != null ? ' (at position ' + r.data.detail.pos + ')' : ''); return; }
    if (r.status !== 200) { err.textContent = 'Could not load issues'; return; }
    meta.textContent = (r.data.total_capped ? r.data.total + '+' : r.data.total) + ' issues';
    if (r.data.warning) meta.append(' · ', h('span', { class: 'note', role: 'status' }, r.data.warning)); // the index is behind, or a fallback answered
    if (!r.data.issues.length) out.append(h('div', { class: 'row' }, h('span', { class: 'muted' }, 'No issues match.')));
    for (const i of r.data.issues) {
      const open = () => { location.hash = '#/issue/' + i.key; };
      out.append(h('div', { class: 'row', role: 'listitem', tabindex: 0, onclick: open, onkeydown: e => { if (e.key === 'Enter') open(); } },
        h('span', { class: 'tile ' + i.type, title: i.type }), h('span', { class: 'key' }, i.key), h('span', { class: 'sum' }, i.summary),
        h('span', { class: 'pri ' + i.priority, title: i.priority }, PRI[i.priority] || ''), i.assignee ? avatar(i.assignee) : h('span', { class: 'av' }, '–'),
        lozenge(i.status, i.status_category), h('span', { class: 'muted' }, L.relTime(i.updated))));
    }
  }

  function createDialog() {
    const proj = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key }, p.key + ' · ' + p.name)));
    const type = h('select', { 'aria-label': 'Type' }, ['Task', 'Bug', 'Story', 'Epic'].map(t => h('option', {}, t)));
    const sum = h('input', { type: 'text', placeholder: 'Summary', 'aria-label': 'Summary', style: 'width:100%' });
    const desc = h('textarea', { placeholder: 'Description', 'aria-label': 'Description' }), msg = h('div', { class: 'err', role: 'alert' });
    const close = modal(h('div', {}, h('h2', {}, 'Create issue'), h('div', { class: 'chips' }, proj, type), sum, h('div', { style: 'height:8px' }), desc, msg,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
        h('button', { class: 'btn primary', onclick: async () => {
          const r = await api('POST', '/api/v1/issues', { project: proj.value, type: type.value, summary: sum.value, description: desc.value });
          if (r.status === 201) { close(); location.hash = '#/issue/' + r.data.key; } else msg.textContent = r.data.error;
        } }, 'Create'))));
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
    document.addEventListener('keydown', key);
    document.body.append(scrim);
    const first = scrim.querySelector('input,textarea,button'); if (first) first.focus();
    close.scrim = scrim;
    return close;
  }

  // ---------- ticket thread ----------
  const draftKey = (key, f) => 'tl-draft:' + S.me.id + ':' + key + ':' + f;
  function localDraft(key, f) { try { return JSON.parse(localStorage.getItem(draftKey(key, f))); } catch (e) { return null; } }
  function setLocalDraft(key, f, v) { try { if (v) localStorage.setItem(draftKey(key, f), JSON.stringify(v)); else localStorage.removeItem(draftKey(key, f)); } catch (e) { /* storage unavailable */ } }

  async function issueView(requestedKey) {
    let key = requestedKey;
    stopTimers();
    const root = h('div', { class: 'page' });
    clear($app).append(topbar(), root);
    let v = await load();
    async function load() {
      const r = await api('GET', '/api/v1/issues/' + key + '/view');
      if (r.status === 410) { clear(root).append(h('div', { class: 'banner danger', role: 'alert' }, 'This issue was deleted. ', h('a', { href: '#/trash/' + key.split('-')[0] }, 'Open the trash'))); return null; }
      if (r.status === 404) { clear(root).append(h('p', {}, 'Issue not found, or you do not have access.')); return null; }
      return r.data;
    }
    if (!v) return;
    const st = { v, editing: null, dirty: false, saveId: null, serverTimer: 0, localTimer: 0, changedBy: null, deleted: null };
    if (v.alias && v.issue.key !== key) { key = v.issue.key; history.replaceState(null, '', '#/issue/' + key); } // an old key redirects to the current one
    S.view = st;
    render();

    function canEdit() { return !v.mirror && (v.role === 'member' || v.role === 'admin'); } // a mirrored issue is edited in Jira

    function render() {
      const is = v.issue;
      clear(root);
      // header: presence stack
      root.append(h('div', { class: 'chips' },
        h('button', { class: 'btn ghost', onclick: () => history.back(), 'aria-label': 'Back' }, '←'),
        h('span', { class: 'key' }, is.key), h('span', { class: 'grow' }), h('span', { id: 'pres', class: 'chips', style: 'margin:0' }),
        canEdit() ? h('button', { class: 'btn', 'aria-haspopup': 'dialog', onclick: actionsMenu }, 'Actions ▾') : null));
      if (st.deleted) root.append(h('div', { class: 'banner danger', role: 'alert' }, 'This issue was deleted' + (st.deleted.by ? ' by ' + st.deleted.by : '') + '. Anything you were typing stays in your draft. ', h('a', { href: '#/trash/' + is.project }, 'Open the trash')));
      fillPresence();
      if (v.mirror) {
        const m = v.mirror, ago = m.synced_at ? L.relTime(m.synced_at) : 'not yet';
        root.append(h('div', { class: 'banner ' + (m.stale ? 'warn' : 'info'), role: 'status' },
          'Mirrored from Jira and read-only here. Synced ' + ago + (m.stale && m.error ? ' (the last sync failed: ' + m.error + ')' : '') + '. ',
          h('a', { href: m.site_url.replace(/\/$/, '') + '/browse/' + encodeURIComponent(m.issue_key || is.key), rel: 'noopener noreferrer', target: '_blank' }, 'Edit in Jira')));
      }
      root.append(subjectEl(is));
      root.append(chipLine(is));
      root.append(descriptionEl(is));
      root.append(attachmentsEl());
      root.append(developmentEl());
      root.append(threadEl());
      root.append(replyEl());
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
    function deleteDialog() {
      lifecycleDialog(key, 'delete', 'Delete ' + v.issue.key, [], () => ({}), () => { toast('Moved to the trash. It can be restored for 30 days.'); location.hash = '#/'; });
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
        const r = await api('POST', '/api/v1/issues/' + key + '/transitions', { transition: trans.value, base_version: is.version });
        if (r.status === 200) await reloadAll(); else scalarError(r, () => api('POST', '/api/v1/issues/' + key + '/transitions', { transition: trans.value }).then(reloadAll));
      });
      const pri = h('select', { 'aria-label': 'Priority', disabled: !canEdit() }, Object.keys(PRI).map(p => h('option', { value: p, selected: p === is.priority }, PRI[p] + ' ' + p)));
      pri.addEventListener('change', () => patch({ priority: pri.value }, 'priority'));
      const asg = h('select', { 'aria-label': 'Assignee', disabled: !canEdit() }, h('option', { value: '' }, 'Unassigned'),
        S.users.map(u => h('option', { value: u.id, selected: is.assignee && is.assignee.id === u.id }, u.name)));
      asg.addEventListener('change', () => patch({ assignee: asg.value }, 'assignee'));
      const labels = h('input', { type: 'text', value: (is.labels || []).join(' '), placeholder: 'labels', 'aria-label': 'Labels', disabled: !canEdit(), style: 'width:160px' });
      labels.addEventListener('change', () => patch({ labels: labels.value.split(/[\s,]+/).filter(Boolean) }, 'labels'));
      return h('div', { class: 'chips' }, lozenge(is.status, is.status_category), trans, pri, asg, labels,
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
      const ta = h('textarea', { 'aria-label': 'Description editor', value: ed.body });
      const state = h('span', { class: 'state', role: 'status' }, st.dirty ? 'Draft saved' : '');
      box.append(h('div', { class: 'chips' }, h('strong', {}, 'Description'), h('span', { class: 'grow' })), h('div', { id: 'editing-warning' }));
      if (ed.restored) box.append(h('div', { class: 'banner warn' }, 'Restored your unsaved draft from ' + L.relTime(ed.restored.at) + '.',
        h('button', { class: 'btn', onclick: () => { ta.value = v.issue.description; ed.body = ta.value; ed.baseSeq = v.issue.description_seq; ed.restored = null; setLocalDraft(key, 'description', null); render(); } }, 'Discard')));
      box.append(h('div', { id: 'changed-banner' }), ta);
      showChanged();
      ta.addEventListener('input', () => {
        ed.body = ta.value; st.dirty = true; state.textContent = 'Editing…';
        clearTimeout(st.localTimer);
        st.localTimer = setTimeout(() => { setLocalDraft(key, 'description', { body: ed.body, base_seq: ed.baseSeq, at: new Date().toISOString() }); state.textContent = 'Draft saved'; }, 2000);
      });
      clearInterval(st.serverTimer);
      st.serverTimer = setInterval(() => { if (st.dirty && st.editing) api('PUT', '/api/v1/issues/' + key + '/drafts/description', { body: ed.body, base_seq: ed.baseSeq }); }, 10000);
      const save = async () => {
        clearTimeout(st.localTimer); setLocalDraft(key, 'description', { body: ed.body, base_seq: ed.baseSeq, at: new Date().toISOString() }); // never lose it
        state.textContent = 'Saving…';
        st.saveId = st.saveId || L.uuid();
        const r = await saveText('description', ed.body, ed.baseSeq, 'save', st.saveId);
        if (r.ok) { stopEditing(true); await reloadAll(); }
        else if (r.conflict) { st.saveId = null; await conflictDialog('description', r.conflict, ed.body, async () => { stopEditing(true); await reloadAll(); }); }
        else { state.textContent = r.msg || 'Offline, draft kept'; } // same save id on retry: a replay can never apply twice
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
      clearInterval(st.serverTimer); clearTimeout(st.localTimer);
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
        let ok = false;
        if (f.size <= 8 << 20) {
          const fd = new FormData(); fd.append('file', f, f.name || 'pasted.png');
          const r = await fetch('/api/v1/issues/' + key + '/attachments', { method: 'POST', body: fd, credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } });
          ok = r.status === 201; if (!ok) toast((await r.json().catch(() => ({}))).error || 'Upload failed');
        } else {
          const d = await direct(f); // straight to the object store when the installation has one; null means use the server
          ok = d === null ? await resumable(f) : d;
        }
        if (ok) await reloadAll();
      }
    }
    // Direct upload (S3 backend): presigned PUTs, three parts at a time, each retried; the server verifies at completion.
    async function direct(f) {
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
    async function resumable(f) {
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
          h('div', {}, h('span', { class: 'who' }, c.author.name), ' ', h('span', { class: 'muted' }, L.relTime(c.created) + (c.internal ? ' · internal' : '') + (c.edited ? ' · edited' : ''))), paragraphs(c.body))));
      }
      if (!v.comments.length) t.append(h('p', { class: 'muted' }, 'No comments yet.'));
      const act = h('div', {}); t.append(act);
      api('GET', '/api/v1/issues/' + key + '/history').then(r => {
        if (r.status !== 200) return;
        for (const x of r.data.filter(x => !['description', 'summary'].includes(x.field) || true).slice(-12)) {
          const line = x.field === 'created' ? 'created this issue' : x.field === 'attachment' ? (x.after ? 'attached ' + x.after : 'removed ' + x.before) : 'changed ' + x.field + (x.before != null && x.after != null && x.field !== 'description' ? ' ' + x.before + ' → ' + x.after : '');
          act.append(h('div', { class: 'activity' }, '·· ' + x.actor + ' ' + line + ' · ' + L.relTime(x.at)));
        }
      });
      return t;
    }
    function replyEl() {
      if (!canEdit()) return h('div', { class: 'muted' }, 'You have read-only access to this project.');
      const ta = h('textarea', { placeholder: 'Reply…', 'aria-label': 'Reply' });
      const ld = localDraft(key, 'comment'); if (ld) ta.value = ld.body;
      let t; ta.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => setLocalDraft(key, 'comment', ta.value ? { body: ta.value, base_seq: 0, at: new Date().toISOString() } : null), 2000); });
      const internal = h('input', { type: 'checkbox', id: 'int' });
      return h('div', { class: 'reply' }, ta, h('div', { class: 'chips' }, h('label', {}, internal, ' Internal'), h('span', { class: 'grow' }),
        h('button', { class: 'btn primary', onclick: async () => {
          if (!ta.value.trim()) return;
          const r = await api('POST', '/api/v1/issues/' + key + '/comments', { body: ta.value, internal: internal.checked });
          if (r.status === 201) { setLocalDraft(key, 'comment', null); await reloadAll(); } else toast(r.data.error || 'Could not send');
        } }, 'Send')));
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
    window.onbeforeunload = null;
    if (!S.me) return loginView();
    const hash = location.hash || '#/';
    let m;
    if ((m = hash.match(/^#\/trash\/([A-Za-z0-9]*)/))) return trashView(m[1].toUpperCase() || (S.projects[0] && S.projects[0].key) || '');
    if ((m = hash.match(/^#\/issue\/([A-Za-z0-9-]+)/))) return issueView(m[1].toUpperCase());
    for (const x of EXT.routes) { const mm = hash.match(x.re); if (mm) { stopTimers(); return x.run(mm); } }
    const q = hash.startsWith('#/?q=') ? decodeURIComponent(hash.slice(5)) : '';
    return listView(q);
  }
  async function boot() {
    const r = await api('GET', '/api/v1/bootstrap');
    if (r.status === 200) { S.me = r.data.me; S.projects = r.data.projects; S.users = r.data.users; rtConnect(); } else S.me = null;
    route();
  }
  // the agile views (agile.js) plug in here
  const EXT = { routes: [] };
  window.TLApp = { h, clear, api, toast, topbar, modal, S, EXT, avatar, lozenge, PRI, timer: (fn, ms) => S.timers.push(setInterval(fn, ms)), route };
  window.addEventListener('hashchange', route);
  boot();
})();
