// The Tasklane web UI, unchanged, running against a pretend server in the browser (for the GitHub Pages mock).
// Loaded before the app's own scripts: it replaces fetch for /api/v1/ with answers built from data.js, which was recorded from a
// real server (scripts/mockweb/). Reads come from that recording; the common changes (transitions, fields, text, comments,
// reactions, board moves, sprint planning, bulk edits, settings, inbox) are applied to an in-memory copy, so they show
// everywhere until the tab is reloaded. Anything else answers "not in the demo". Realtime is off, as on a server without it.
(function () {
  'use strict';
  const D = window.TL_MOCK_DATA;
  if (!D) return;

  // ---------- the recording, with its timestamps moved so that "recorded at" becomes "now". The sample data was made by a script
  // in about a minute; what happened in the hour before the recording is spread over the last three days, in the same order, so
  // the lists read "2 days ago", "5 hours ago" rather than all the same minute. Other times (sprint dates) only move.
  const rec = Date.parse(D.recorded_at), shift = Date.now() - rec;
  const ISO = /"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))"/g;
  const raw = JSON.stringify(D.fixtures), HOUR = 3600e3, SPREAD = 3 * 24 * HOUR, NEWEST = 20 * 60e3;
  const recent = [...new Set([...raw.matchAll(ISO)].map(m => Date.parse(m[1])).filter(t => rec - t >= 0 && rec - t < HOUR))].sort((a, b) => b - a);
  const rank = new Map(recent.map((t, i) => [t, i])); // by rank, not by distance: evenly spread, newest first
  const moved = t => rank.has(t) ? Date.now() - NEWEST - rank.get(t) / Math.max(1, recent.length - 1) * SPREAD : t + shift;
  const FX = JSON.parse(raw.replace(ISO, (m, s) => '"' + new Date(moved(Date.parse(s))).toISOString() + '"'));
  const clone = o => o === undefined ? undefined : JSON.parse(JSON.stringify(o));
  const now = () => new Date().toISOString();
  const fx = k => FX['GET ' + k] && FX['GET ' + k].body;

  // ---------- the model
  const boot = fx('/api/v1/bootstrap');
  const M = {
    signedIn: true,
    me: boot.me,
    users: boot.users,
    projects: boot.projects,
    prefs: boot.ui_prefs,
    views: {}, history: {}, versions: {}, watch: {}, order: [], sprintOf: {}, sprints: {}, boards: {},
    notes: clone(fx('/api/v1/notifications?limit=50')) || [],
    nextComment: 1, undo: {}, nextUndo: 1,
  };
  const meUser = () => M.users.find(u => u.id === M.me.id) || { id: M.me.id, name: M.me.label, email: '', kind: 'human', is_admin: true };
  const person = u => u ? { id: u.id, email: u.email, name: u.name, kind: '', is_admin: false } : null;
  for (const k of Object.keys(FX)) {
    let m;
    if ((m = k.match(/^GET \/api\/v1\/issues\/([A-Z]+-\d+)\/view$/))) M.views[m[1]] = FX[k].body;
    else if ((m = k.match(/^GET \/api\/v1\/issues\/([A-Z]+-\d+)\/history$/))) M.history[m[1]] = FX[k].body;
    else if ((m = k.match(/^GET \/api\/v1\/issues\/([A-Z]+-\d+)\/text\/description\/versions$/))) M.versions[m[1]] = FX[k].body;
    else if ((m = k.match(/^GET \/api\/v1\/issues\/([A-Z]+-\d+)\/watch$/))) M.watch[m[1]] = FX[k].body;
    else if ((m = k.match(/^GET \/api\/v1\/boards\/([0-9a-f-]{36})$/))) M.boards[m[1]] = FX[k].body;
  }
  const keyNum = k => [k.split('-')[0], Number(k.split('-')[1])];
  const byKey = (a, b) => { const [pa, na] = keyNum(a), [pb, nb] = keyNum(b); return pa < pb ? -1 : pa > pb ? 1 : na - nb; };
  M.order = Object.keys(M.views).sort(byKey);
  // sprints and who is in them, from the recorded backlog of each scrum board
  for (const k of Object.keys(FX)) {
    const m = k.match(/^GET \/api\/v1\/boards\/([0-9a-f-]{36})\/backlog$/);
    if (!m) continue;
    for (const s of FX[k].body.sprints) {
      M.sprints[s.id] = Object.assign(clone(s), { issues: undefined });
      for (const i of s.issues) M.sprintOf[i.key] = s.id;
    }
  }
  // the workflow as the recording saw it: from each status, the transitions out of it, per project
  const flow = {};
  for (const v of Object.values(M.views)) {
    const p = (flow[v.issue.project] = flow[v.issue.project] || {});
    if (!p[v.issue.status]) p[v.issue.status] = clone(v.transitions);
  }
  const catOf = {};
  for (const p of Object.values(flow)) for (const ts of Object.values(p)) for (const t of ts) catOf[t.to] = t.to_category;
  const transitionsFor = is => clone((flow[is.project] || {})[is.status] || []);

  // ---------- helpers
  const ok = (body, status, headers) => ({ status: status || 200, body, headers });
  const err = (status, msg, code) => ({ status, body: { error: msg, code: code || 'mock' } });
  const NOT_HERE = () => err(501, 'This is a demo with sample data: that action is not available here. Everything else you see is the real Tasklane UI.', 'not_in_demo');
  const userById = id => M.users.find(u => u.id === id);
  const userByName = n => M.users.find(u => u.name.toLowerCase() === String(n).toLowerCase());
  function touch(key, field, before, after) {
    const v = M.views[key], is = v.issue;
    is.version++; is.updated = now();
    (M.history[key] = M.history[key] || []).push({ field, before: before === undefined ? null : before, after: after === undefined ? null : after, actor: meUser().name, at: is.updated });
  }
  // a change can be undone for a short while: the whole issue as it was is kept under a token (the real server is stricter)
  function undoable(keys) {
    const t = 'mock-' + (M.nextUndo++);
    M.undo[t] = keys.map(k => ({ key: k, view: clone(M.views[k]), hist: clone(M.history[k] || []), sprint: M.sprintOf[k] }));
    return { 'Tasklane-Undo': t };
  }
  function transition(key, tid) {
    const v = M.views[key];
    const t = v.transitions.find(x => x.id === tid || x.name === tid);
    if (!t) return err(409, 'That transition is not available from ' + v.issue.status + '.');
    const before = v.issue.status;
    v.issue.status = t.to; v.issue.status_category = t.to_category;
    touch(key, 'status', before, t.to);
    v.transitions = transitionsFor(v.issue);
    return ok(clone(v.issue));
  }
  function setFields(key, ch) {
    const is = M.views[key].issue;
    if (ch.priority !== undefined && ch.priority !== is.priority) { touch(key, 'priority', is.priority, ch.priority); is.priority = ch.priority; }
    if (ch.assignee !== undefined) {
      const was = is.assignee ? is.assignee.name : '', u = ch.assignee ? userById(ch.assignee) : null;
      if ((u ? u.name : '') !== was) { is.assignee = person(u); touch(key, 'assignee', was, u ? u.name : ''); }
    }
    if (ch.labels !== undefined) { const b = (is.labels || []).join(' '); is.labels = [...new Set(ch.labels)].sort(); touch(key, 'labels', b, is.labels.join(' ')); }
    if (ch.labels_add) { const b = (is.labels || []).join(' '); is.labels = [...new Set((is.labels || []).concat(ch.labels_add))].sort(); touch(key, 'labels', b, is.labels.join(' ')); }
    if (ch.labels_remove) { const b = (is.labels || []).join(' '); is.labels = (is.labels || []).filter(l => !ch.labels_remove.includes(l)); touch(key, 'labels', b, is.labels.join(' ')); }
    if (ch.estimate !== undefined) { const b = is.estimate == null ? '' : String(is.estimate); is.estimate = ch.estimate === null || ch.estimate === '' ? undefined : Number(ch.estimate); touch(key, 'estimate', b, is.estimate == null ? '' : String(is.estimate)); }
    if (ch.custom) { is.custom = Object.assign(is.custom || {}, ch.custom); touch(key, 'custom', null, null); }
  }
  const visible = () => M.order.filter(k => M.views[k] && !M.views[k].issue.deleted);
  const childCount = key => visible().filter(k => M.views[k].issue.parent === key);
  function card(key) {
    const is = M.views[key].issue;
    const c = { key, summary: is.summary, type: is.type, status: is.status, priority: is.priority, assignee: is.assignee ? is.assignee.name : '', labels: is.labels || [],
      rank: String(M.order.indexOf(key)).padStart(8, '0'), category: is.status_category };
    if (is.parent) c.parent = is.parent;
    if (is.estimate != null) c.estimate = is.estimate;
    const kids = childCount(key);
    if (kids.length) { c.subtasks = kids.length; c.subtasks_done = kids.filter(k => M.views[k].issue.status_category === 'done').length; }
    return c;
  }
  function totals(keys) {
    const t = { todo: { count: 0, estimate: 0 }, in_progress: { count: 0, estimate: 0 }, done: { count: 0, estimate: 0 } };
    for (const k of keys) { const is = M.views[k].issue, x = t[is.status_category]; if (x) { x.count++; x.estimate += is.estimate || 0; } }
    return t;
  }
  function place(key, pos, inList) { // rank: move key right before/after another, or to the top of the list it is in
    const o = M.order, i = o.indexOf(key);
    if (i >= 0) o.splice(i, 1);
    let at = o.length;
    if (pos.before && o.includes(pos.before)) at = o.indexOf(pos.before);
    else if (pos.after && o.includes(pos.after)) at = o.indexOf(pos.after) + 1;
    else if (pos.top && inList && inList.length) at = Math.min(...inList.filter(k => k !== key).map(k => o.indexOf(k)).filter(n => n >= 0), o.length);
    o.splice(at, 0, key);
  }

  // ---------- search: plain text (words, "phrases", -word, OR) and a useful part of TQL
  const FIELD = {
    project: is => is.project, status: is => is.status, type: is => is.type, issuetype: is => is.type, priority: is => is.priority,
    assignee: is => is.assignee ? is.assignee.name : '', reporter: is => is.reporter ? is.reporter.name : '', labels: is => is.labels || [], label: is => is.labels || [],
    key: is => is.key, parent: is => is.parent || '', epic: is => is.parent || '', summary: is => is.summary, description: is => is.description, text: is => is.summary + ' ' + is.description,
    statuscategory: is => ({ todo: 'To Do', in_progress: 'In Progress', done: 'Done' })[is.status_category],
    sprint: is => M.sprintOf[is.key] ? M.sprints[M.sprintOf[is.key]].name : '',
  };
  const hay = v => { const is = v.issue; return [is.key, is.summary, is.description, is.assignee && is.assignee.name, is.reporter && is.reporter.name, (is.labels || []).join(' '), is.status, is.type, ...v.comments.map(c => c.body)].join(' \n ').toLowerCase(); };
  function textMatcher(q) {
    const groups = q.split(/\s+OR\s+/).map(g => (g.match(/-?"[^"]*"|\S+/g) || []).filter(t => t !== 'AND' && t !== '(' && t !== ')').map(t => t.replace(/[()]/g, '')));
    return v => { const s = hay(v); return groups.some(g => g.every(t => { const neg = t[0] === '-' || t.startsWith('NOT'); const w = t.replace(/^-|^NOT/, '').replace(/"/g, '').toLowerCase(); return !w || (s.includes(w) !== neg); })); };
  }
  function tqlMatcher(q) {
    const body = q.replace(/\s+ORDER\s+BY\s+.*$/i, '').trim();
    if (!body) return () => true;
    const ors = body.split(/\s+OR\s+/i).map(part => part.split(/\s+AND\s+/i).map(c => {
      c = c.trim();
      const bal = s => (s.match(/\(/g) || []).length - (s.match(/\)/g) || []).length;
      while (c.startsWith('(') && bal(c) > 0) c = c.slice(1).trim(); // grouping parentheses: the demo reads AND / OR left to right
      while (c.endsWith(')') && bal(c) < 0) c = c.slice(0, -1).trim();
      const m = c.match(/^([A-Za-z_]+)\s*(!=|=|~|!~|\bnot in\b|\bin\b|\bis not\b|\bis\b)\s*(.+)$/i);
      if (!m) throw new Error('Could not read "' + c + '". The demo understands field = value, !=, ~, in (…) and is EMPTY, joined by AND / OR.');
      const f = FIELD[m[1].toLowerCase()];
      if (!f) throw new Error('Unknown field "' + m[1] + '". Try project, status, type, priority, assignee, reporter, labels, sprint, parent or text.');
      const op = m[2].toLowerCase();
      let raw = m[3].trim();
      const unq = s => s.trim().replace(/^["']|["']$/g, '');
      const me = s => /^currentuser\(\)$/i.test(s) ? meUser().name : s;
      const vals = /in$/.test(op) ? raw.replace(/^\(|\)$/g, '').split(',').map(unq).map(me) : [me(unq(raw))];
      return is => {
        const got = f(is), list = (Array.isArray(got) ? got : [got]).map(x => String(x || '').toLowerCase());
        if (op === 'is' || op === 'is not') { const empty = list.every(x => !x); return op === 'is' ? empty : !empty; }
        if (op === '~' || op === '!~') { const hit = list.some(x => x.includes(vals[0].toLowerCase())); return op === '~' ? hit : !hit; }
        const hit = vals.some(w => list.includes(w.toLowerCase()));
        return op === '=' || op === 'in' ? hit : !hit;
      };
    }));
    return v => ors.some(ands => ands.every(t => t(v.issue)));
  }
  const PRI_ORDER = { highest: 0, high: 1, medium: 2, low: 3, lowest: 4 };
  function search(u) {
    const q = u.searchParams.get('q') || '', syntax = u.searchParams.get('syntax') || 'tql';
    let match;
    try { match = syntax === 'text' ? textMatcher(q) : tqlMatcher(q); } catch (e) { return err(400, e.message, 'bad_query'); }
    let rows = visible().map(k => M.views[k]).filter(v => !v.issue.archived && match(v));
    const [sf, sd] = (u.searchParams.get('sort') || 'updated:desc').split(':');
    const sv = v => { const is = v.issue; switch (sf) {
      case 'key': return keyNum(is.key)[0] + String(keyNum(is.key)[1]).padStart(6, '0'); case 'priority': return PRI_ORDER[is.priority];
      case 'assignee': return is.assignee ? is.assignee.name : '~'; case 'reporter': return is.reporter.name; default: return is[sf] == null ? '' : is[sf]; } };
    rows.sort((a, b) => { const x = sv(a), y = sv(b); const c = x < y ? -1 : x > y ? 1 : 0; return (sd === 'desc' ? -c : c) || byKey(b.issue.key, a.issue.key); });
    const total = rows.length, limit = Number(u.searchParams.get('limit')) || 50, off = Number(u.searchParams.get('offset')) || 0;
    const fields = (u.searchParams.get('fields') || 'type,key,summary,priority,assignee,status,updated,project').split(',');
    const snip = Number(u.searchParams.get('snippet')) || 500;
    const issues = rows.slice(off, off + limit).map(v => {
      const is = v.issue, o = { key: is.key, status_category: is.status_category };
      for (const f of fields) {
        if (f === 'assignee' || f === 'reporter') o[f] = is[f] ? is[f].name : '';
        else if (f === 'comments') o.comments = v.comments.length;
        else if (f === 'snippet') o.snippet = (is.description || '').slice(0, snip);
        else if (f.startsWith('customfield_')) o[f] = (is.custom || {})[f];
        else o[f] = is[f];
      }
      return o;
    });
    return ok({ engine: '', fields, issues, query: syntax === 'text' ? '' : q, total, total_capped: false, warning: '' });
  }

  // ---------- the routes
  const R = [];
  const on = (method, re, fn) => R.push({ method, re, fn });
  on('POST', /^\/api\/v1\/login$/, () => { M.signedIn = true; return ok({ ok: true }); });
  on('POST', /^\/api\/v1\/logout$/, () => { M.signedIn = false; return ok({ ok: true }); });
  on('POST', /^\/api\/v1\/ws\/ticket$/, () => err(404, 'realtime is off in the demo'));
  on('GET', /^\/api\/v1\/bootstrap$/, () => M.signedIn ? ok(Object.assign(clone(boot), { ui_prefs: clone(M.prefs) })) : err(401, 'sign in', 'unauthenticated'));
  on('PUT', /^\/api\/v1\/me\/ui-prefs$/, (m, u, b) => { Object.assign(M.prefs.prefs, b); M.prefs.version++; return ok(clone(M.prefs)); });
  on('GET', /^\/api\/v1\/issues$/, (m, u) => search(u));
  on('GET', /^\/api\/v1\/search\/typeahead$/, (m, u) => {
    const q = (u.searchParams.get('q') || '').toLowerCase();
    return ok(visible().map(k => M.views[k].issue).filter(is => (is.key + ' ' + is.summary).toLowerCase().includes(q)).slice(0, 8)
      .map(is => ({ key: is.key, summary: is.summary, type: is.type, status: is.status, status_category: is.status_category, project: is.project })));
  });
  // one issue
  const K = '([A-Z][A-Z0-9]*-\\d+)';
  const issueRoute = (method, tail, fn) => on(method, new RegExp('^/api/v1/issues/' + K + tail + '$'), (m, u, b, h) => M.views[m[1]] ? fn(m[1], m, u, b, h) : err(404, 'Issue ' + m[1] + ' does not exist, or you cannot see it.', 'not_found'));
  issueRoute('GET', '/view', k => ok(clone(M.views[k])));
  issueRoute('GET', '/history', k => ok(clone(M.history[k] || [])));
  issueRoute('GET', '/text/description/versions', k => ok(clone(M.versions[k] || [])));
  issueRoute('GET', '/text/summary/versions', () => ok([]));
  issueRoute('GET', '/drafts', () => ok([]));
  issueRoute('PUT', '/drafts/([a-z]+)', () => ok({ ok: true }));
  issueRoute('DELETE', '/drafts/([a-z]+)', () => ok({ ok: true }));
  issueRoute('GET', '/development', () => ok({ summary: '', pull_requests: [], branches: [], commits: [], builds: [], deployments: [] }));
  issueRoute('GET', '/approvals', () => ok([]));
  issueRoute('GET', '/watch', k => ok(clone(M.watch[k] || { watching: false, explicit: false, muted: false, watchers: 0 })));
  issueRoute('PUT', '/watch', (k, m, u, b) => { M.watch[k] = { watching: !!b.watch, explicit: true, muted: false, watchers: b.watch ? 1 : 0 }; return ok(clone(M.watch[k])); });
  issueRoute('POST', '/presence', () => ok([]));
  issueRoute('DELETE', '/presence', () => ok([]));
  issueRoute('POST', '/transitions', (k, m, u, b, h) => { const hd = h['Tasklane-Want-Undo'] ? undoable([k]) : undefined; const r = transition(k, b.transition); r.headers = hd; return r; });
  issueRoute('PATCH', '', (k, m, u, b) => { setFields(k, b); return ok(clone(M.views[k].issue)); });
  issueRoute('PUT', '/text/(summary|description)', (k, m, u, b) => {
    const f = m[2], is = M.views[k].issue, before = is[f];
    if (f === 'summary' && !String(b.body).trim()) return err(400, 'The summary cannot be empty.');
    is[f] = b.body; is[f + '_seq']++;
    touch(k, f, f === 'summary' ? before : null, f === 'summary' ? b.body : null);
    if (f === 'description') (M.versions[k] = M.versions[k] || []).push({ seq: is.description_seq, body: b.body, actor: meUser().name, mode: b.mode || 'edit', at: is.updated });
    return ok({ seq: is[f + '_seq'], removed: false });
  });
  issueRoute('POST', '/comments', (k, m, u, b) => {
    if (!String(b.body || '').trim()) return err(400, 'A comment cannot be empty.');
    const c = { id: 'mock-c' + (M.nextComment++), author: person(meUser()), body: b.body, internal: !!b.internal, created: now() };
    M.views[k].comments.push(c); M.views[k].issue.updated = c.created;
    return ok(clone(c), 201);
  });
  issueRoute('PATCH', '/comments/([^/]+)', (k, m, u, b) => { const c = M.views[k].comments.find(x => x.id === m[2]); if (!c) return err(404, 'No such comment.'); c.body = b.body; c.edited = now(); return ok({ ok: true }); });
  issueRoute('PUT', '/comments/([^/]+)/reactions', (k, m, u, b) => {
    const c = M.views[k].comments.find(x => x.id === m[2]);
    if (!c) return err(404, 'No such comment.');
    const me = meUser().name, list = c.reactions = c.reactions || [];
    let r = list.find(x => x.emoji === b.emoji);
    if (b.on && !(r && r.mine)) { if (!r) list.push(r = { emoji: b.emoji, count: 0, names: [], mine: false }); r.count++; r.names.push(me); r.mine = true; }
    if (!b.on && r && r.mine) { r.count--; r.names = r.names.filter(n => n !== me); r.mine = false; if (!r.count) list.splice(list.indexOf(r), 1); }
    return ok(clone(list));
  });
  // a new issue
  on('POST', /^\/api\/v1\/issues$/, (m, u, b) => {
    const p = M.projects.find(x => x.key === b.project);
    if (!p) return err(400, 'Choose a project.');
    if (!String(b.summary || '').trim()) return err(400, 'The summary is required.');
    const n = Math.max(0, ...Object.keys(M.views).filter(k => k.startsWith(p.key + '-')).map(k => keyNum(k)[1])) + 1, key = p.key + '-' + n;
    const t = now(), first = (flow[p.key] && flow[p.key]['To Do']) ? 'To Do' : Object.keys(flow[p.key] || { 'To Do': 1 })[0];
    const is = { id: 'mock-' + key, key, project: p.key, type: b.type || 'Task', summary: b.summary.trim(), summary_seq: 1, description: b.description || '', description_seq: 1,
      status: first, status_category: catOf[first] || 'todo', priority: b.priority || 'medium', reporter: person(meUser()), assignee: person(userById(b.assignee)),
      labels: b.labels || [], version: 1, created: t, updated: t };
    if (b.parent) is.parent = b.parent;
    M.views[key] = { issue: is, transitions: transitionsFor(is), comments: [], attachments: [], links: [], role: p.role, presence: [], versions: { fix: [], affects: [] }, approvals: [] };
    M.history[key] = [{ field: 'created', before: null, after: key, actor: meUser().name, at: t }];
    M.versions[key] = [{ seq: 1, body: is.description, actor: meUser().name, mode: 'create', at: t }];
    M.watch[key] = { watching: true, explicit: false, muted: false, watchers: 0 };
    M.order.push(key);
    return ok(clone(is), 201, { Location: '/api/v1/issues/' + key + '/view' });
  });
  on('POST', /^\/api\/v1\/undo\/(.+)$/, m => {
    const u = M.undo[decodeURIComponent(m[1])];
    if (!u) return err(404, 'It is too late to undo that.');
    delete M.undo[decodeURIComponent(m[1])];
    for (const x of u) { M.views[x.key] = x.view; M.history[x.key] = x.hist; if (x.sprint) M.sprintOf[x.key] = x.sprint; else delete M.sprintOf[x.key]; }
    return ok({ ok: true, keys: u.map(x => x.key) });
  });
  // boards
  const boardOf = id => M.boards[id];
  const sprintKeys = sid => visible().filter(k => M.sprintOf[k] === sid);
  on('GET', /^\/api\/v1\/boards\/([0-9a-f-]{36})$/, m => {
    const b = boardOf(m[1]);
    if (!b) return err(404, 'This board does not exist, or you cannot see it.');
    const out = clone(b), proj = b.board.project;
    let keys = visible().filter(k => M.views[k].issue.project === proj && M.views[k].issue.type !== 'Epic');
    if (b.board.kind === 'scrum') { const act = Object.values(M.sprints).find(s => s.board === b.board.id && s.state === 'active'); keys = act ? keys.filter(k => M.sprintOf[k] === act.id) : []; }
    for (const c of out.columns) { c.cards = keys.filter(k => c.statuses.includes(M.views[k].issue.status)).map(card); c.count = c.cards.length; }
    return ok(out);
  });
  on('GET', /^\/api\/v1\/boards\/([0-9a-f-]{36})\/backlog$/, m => {
    const b = boardOf(m[1]);
    if (!b) return err(404, 'This board does not exist, or you cannot see it.');
    const rec = clone(FX['GET /api/v1/boards/' + m[1] + '/backlog'].body);
    for (const s of rec.sprints) { const ks = sprintKeys(s.id); Object.assign(s, M.sprints[s.id], { issues: ks.map(card), totals: totals(ks) }); s.committed_estimate = s.state === 'active' ? s.committed_estimate : undefined; }
    const rest = visible().filter(k => { const is = M.views[k].issue; return is.project === b.board.project && !M.sprintOf[k] && is.status_category !== 'done'; });
    rec.backlog = rest.map(card); rec.backlog_totals = totals(rest);
    return ok(rec);
  });
  on('POST', /^\/api\/v1\/boards\/([0-9a-f-]{36})\/move$/, (m, u, b) => {
    const bd = boardOf(m[1]);
    if (!bd || !M.views[b.key]) return err(404, 'Not found.');
    const col = bd.columns.find(c => c.id === b.column || c.name === b.column);
    if (!col) return err(400, 'No such column.');
    const v = M.views[b.key];
    let res = { ok: true, transitioned: false };
    if (!col.statuses.includes(v.issue.status)) {
      const t = v.transitions.find(x => col.statuses.includes(x.to));
      if (!t) return err(409, 'The workflow has no transition from ' + v.issue.status + ' to ' + col.name + '.');
      transition(b.key, t.id);
      res = { ok: true, transitioned: true, transition: t.name, status: t.to };
    }
    place(b.key, b, null);
    return ok(res);
  });
  on('POST', /^\/api\/v1\/boards\/([0-9a-f-]{36})\/plan$/, (m, u, b) => {
    for (const k of b.keys || []) {
      if (!M.views[k]) continue;
      const was = M.sprintOf[k] ? M.sprints[M.sprintOf[k]].name : 'Backlog';
      if (b.sprint) M.sprintOf[k] = b.sprint; else delete M.sprintOf[k];
      const now_ = b.sprint ? M.sprints[b.sprint].name : 'Backlog';
      if (was !== now_) touch(k, 'sprint', was, now_);
      place(k, b, b.sprint ? sprintKeys(b.sprint) : visible().filter(x => !M.sprintOf[x]));
    }
    return ok({ ok: true });
  });
  on('POST', /^\/api\/v1\/boards\/([0-9a-f-]{36})\/rank$/, (m, u, b) => { const list = M.sprintOf[b.key] ? sprintKeys(M.sprintOf[b.key]) : visible().filter(x => !M.sprintOf[x]); place(b.key, b, list); return ok({ ok: true }); });
  on('PATCH', /^\/api\/v1\/sprints\/([0-9a-f-]{36})$/, (m, u, b) => { const s = M.sprints[m[1]]; if (!s) return err(404, 'No such sprint.'); if (b.name != null) s.name = b.name; if (b.goal != null) s.goal = b.goal; return ok(clone(s)); });
  // inbox
  const notes = st => M.notes.filter(n => st === 'archived' ? n.state === 'archived' : st === 'unread' ? n.state === 'unread' : n.state !== 'archived');
  on('GET', /^\/api\/v1\/notifications$/, (m, u) => ok(clone(notes(u.searchParams.get('state') || ''))));
  on('GET', /^\/api\/v1\/notifications\/unread-count$/, () => ok({ unread: notes('unread').length }));
  on('POST', /^\/api\/v1\/notifications\/(\d+)\/state$/, (m, u, b) => { const n = M.notes.find(x => String(x.id) === m[1]); if (n) n.state = b.state; return ok({ ok: true }); });
  on('POST', /^\/api\/v1\/notifications\/read-all$/, () => { const u = notes('unread'); u.forEach(n => { n.state = 'read'; }); return ok({ marked: u.length }); });
  // bulk: field changes and transitions on the selection
  on('POST', /^\/api\/v1\/bulk\/transitions$/, (m, u, b) => {
    const seen = {};
    for (const k of b.keys || []) for (const t of (M.views[k] ? M.views[k].transitions : [])) { const x = (seen[t.name + '→' + t.to] = seen[t.name + '→' + t.to] || { name: t.name, to: t.to, issues: 0 }); x.issues++; }
    return ok(Object.values(seen));
  });
  on('POST', /^\/api\/v1\/bulk$/, (m, u, b, h) => {
    if (b.op !== 'fields' && b.op !== 'transition') return NOT_HERE();
    const keys = (b.keys || []).filter(k => M.views[k]);
    if (b.dry_run) return ok({ needs_review: false, summary: { changed: keys.length }, items: keys.map(k => ({ key: k, ok: true })) });
    const hd = h['Tasklane-Want-Undo'] ? undoable(keys) : undefined;
    const items = keys.map(k => {
      if (b.op === 'transition') { const r = transition(k, b.transition); return r.status === 200 ? { key: k, ok: true } : { key: k, ok: false, error: r.body.error }; }
      const ch = {}; for (const f of ['priority', 'assignee', 'labels_add', 'labels_remove']) if (b[f] !== undefined) ch[f] = b[f];
      setFields(k, ch); return { key: k, ok: true };
    });
    return ok({ summary: { changed: items.filter(i => i.ok).length, failed: items.filter(i => !i.ok).length }, items }, 200, hd);
  });

  // ---------- dispatch
  function answer(method, u, body, headers) {
    const path = u.pathname.replace(/^.*?(\/api\/v1\/)/, '$1');
    if (!M.signedIn && !/^\/api\/v1\/(login|bootstrap|auth\/|system\/status)/.test(path)) return err(401, 'Sign in first.', 'unauthenticated');
    for (const r of R) { if (r.method !== method) continue; const m = path.match(r.re); if (m) return r.fn(m, u, body || {}, headers); }
    if (method === 'GET') {
      const exact = FX['GET ' + path + u.search] || FX['GET ' + path];
      if (exact) return ok(clone(exact.body), exact.status);
      const same = Object.keys(FX).find(k => k.split('?')[0] === 'GET ' + path); // the same place with other parameters
      if (same) return ok(clone(FX[same].body), FX[same].status);
      return err(404, 'Not available in this demo.', 'not_found');
    }
    return NOT_HERE();
  }
  const realFetch = window.fetch.bind(window);
  window.fetch = async function (input, init) {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (url.pathname.indexOf('/api/v1/') < 0) return realFetch(input, init);
    init = init || {};
    const method = (init.method || 'GET').toUpperCase();
    let body; try { body = typeof init.body === 'string' ? JSON.parse(init.body) : undefined; } catch (e) { body = undefined; }
    const hdrs = Object.assign({}, init.headers || {});
    let r;
    try { r = answer(method, url, body, hdrs); } catch (e) { console.error('[mock]', e); r = err(500, 'The demo could not answer that: ' + e.message); }
    await new Promise(res => setTimeout(res, 40 + Math.random() * 60)); // a little latency, as over a network
    const h = new Headers(Object.assign({ 'Content-Type': 'application/json' }, r.headers || {}));
    return new Response(r.body === undefined || r.status === 204 ? null : JSON.stringify(r.body), { status: r.status, headers: h });
  };
  // the realtime socket is never opened (the ticket answers 404), but keep a stray one from reaching the network
  window.WebSocket = function () { throw new Error('realtime is off in the demo'); };
})();
