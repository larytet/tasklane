// The bulk bar and the row hover actions (SPEC 4.1). The bar appears when issues of the list are selected and acts on up to one
// page of them: assign, transition, label, move, archive, delete. Every action is planned first on the server (the same rules as
// the MCP bulk tools); the plan is shown only when some issues would fail or lose data, otherwise it applies at once and offers
// Undo. Failures are reported issue by issue. "Select all N matching" (a background job) is a later milestone and is not offered.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, api, toast, modal, S } = A;

  const projectOf = key => (S.projects || []).find(p => p.key === key.split('-')[0]);
  const writable = key => { const p = projectOf(key); return !!p && !p.mirrored && (p.role === 'member' || p.role === 'admin'); };

  let bar = null, count = null, why = null;
  function ensureBar() {
    const list = document.querySelector('#app [role=list]');
    if (!list) return null;
    if (bar && bar.isConnected && bar.nextSibling === list) return bar;
    count = h('strong', { 'aria-live': 'polite' });
    why = h('span', { class: 'muted' });
    const btn = (label, fn) => h('button', { class: 'btn', 'data-act': label, onclick: fn }, label);
    bar = h('div', { class: 'bulkbar', role: 'toolbar', 'aria-label': 'Selected issues', hidden: true },
      count, btn('Assign', assignAll), btn('Transition', transitionAll), btn('Label', labelAll), btn('Move', moveAll), btn('Archive', () => run({ op: 'archive' }, 'archived')),
      btn('Delete', () => run({ op: 'delete' }, 'moved to the trash')), why, h('span', { class: 'grow' }), h('button', { class: 'btn ghost', onclick: clearSel }, 'Clear'));
    list.before(bar);
    return bar;
  }
  function update() {
    const b = ensureBar();
    if (!b) return;
    const n = S.sel ? S.sel.size : 0;
    b.hidden = n === 0;
    count.textContent = n + ' selected';
    // a viewer sees the bar with its actions off and the reason, rather than requests that fail (BULK-07)
    const can = [...(S.sel || [])].some(writable);
    for (const x of b.querySelectorAll('button[data-act]')) { x.disabled = !can; x.title = can ? '' : 'You can only view these issues'; }
    why.textContent = n && !can ? 'You can only view these issues, so they cannot be changed.' : '';
  }
  function clearSel() {
    for (const c of document.querySelectorAll('#app [role=list] input.rowsel:checked')) { c.checked = false; c.dispatchEvent(new Event('change')); }
    const all = document.querySelector('input[aria-label="Select all on this page"]');
    if (all) { all.checked = false; all.indeterminate = false; }
  }
  A.EXT.onSelect = update;

  const errText = r => (r.data && r.data.error) || 'The action failed';
  // plan, then apply at once when the plan is clean, or show it first
  async function run(op, done) {
    const keys = [...S.sel];
    if (!keys.length) return;
    const body = Object.assign({ keys }, op);
    const plan = await api('POST', '/api/v1/bulk', Object.assign({ dry_run: true }, body));
    if (plan.status !== 200) { toast(errText(plan)); return; }
    const go = () => apply(body, done);
    if (plan.data.needs_review) showPlan(plan.data, go); else go();
  }
  async function apply(body, done) {
    const r = await api('POST', '/api/v1/bulk', Object.assign({ dry_run: false, idempotency_key: 'bar-' + (window.TL ? window.TL.uuid() : Date.now()) }, body), A.WANT_UNDO);
    if (r.status !== 200) { toast(errText(r)); return; }
    const s = r.data.summary, failed = r.data.items.filter(i => !i.ok);
    A.undoToast(s.changed + (s.changed === 1 ? ' issue ' : ' issues ') + done + '.', r, () => A.route());
    if (failed.length) report(failed, s.changed);
    A.route();
  }
  const itemLine = it => h('li', {}, h('span', { class: 'key' }, it.key), ' ',
    !it.ok ? h('span', { class: 'err' }, it.error) : it.unchanged ? h('span', { class: 'muted' }, 'nothing to change') :
      h('span', {}, (it.changes || []).map(c => c.change || (c.field + ': ' + fmt(c.before) + ' → ' + fmt(c.after))).join('; '),
        (it.drops || []).length ? h('span', { class: 'err' }, ' · drops: ' + it.drops.join('; ')) : null));
  const fmt = v => Array.isArray(v) ? (v.join(', ') || 'none') : (v === '' || v == null ? 'none' : String(v));
  function showPlan(p, go) {
    const ok = p.items.filter(i => i.ok && !i.unchanged).length;
    const close = modal(h('div', { class: 'bulkplan' }, h('h2', {}, 'Check before applying'),
      h('p', {}, ok + ' of ' + p.items.length + ' issues can be changed. ' + (p.summary.errors ? p.summary.errors + ' cannot. ' : '') + (p.summary.with_drops ? p.summary.with_drops + ' would lose data.' : '')),
      h('ul', { class: 'planlist' }, p.items.map(itemLine)),
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
        h('button', { class: 'btn primary', disabled: !ok, onclick: () => { close(); go(); } }, ok === p.items.length ? 'Apply' : 'Apply to ' + ok))));
  }
  function report(failed, changed) {
    const close = modal(h('div', { class: 'bulkplan' }, h('h2', {}, failed.length + (failed.length === 1 ? ' issue was' : ' issues were') + ' not changed'),
      h('p', {}, changed + ' changed. These were left as they are:'), h('ul', { class: 'planlist' }, failed.map(itemLine)),
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Close'))));
  }

  function assignAll() {
    const me = S.me, people = (S.users || []).filter(u => u.id !== me.id).sort((a, b) => a.name.localeCompare(b.name));
    A.picker('Assign ' + S.sel.size + ' issues', [{ label: 'Me', always: true, run: () => run({ op: 'fields', assignee: 'me' }, 'assigned to you') },
      { label: 'Unassigned', always: true, run: () => run({ op: 'fields', assignee: '' }, 'unassigned') }]
      .concat(people.map(u => ({ label: u.name, run: () => run({ op: 'fields', assignee: u.id }, 'assigned to ' + u.name) }))), { searchLabel: 'Search people' });
  }
  async function transitionAll() {
    // only the transitions some of the selection can take now
    const r = await api('POST', '/api/v1/bulk/transitions', { keys: [...S.sel] });
    if (r.status !== 200) { toast(errText(r)); return; }
    if (!r.data.length) { toast('None of the selected issues can take a transition now.'); return; }
    A.picker('Transition ' + S.sel.size + ' issues', r.data.map(t => ({ label: t.name + ' → ' + t.to + (t.issues < S.sel.size ? ' (' + t.issues + ' of ' + S.sel.size + ')' : ''), run: () => run({ op: 'transition', transition: t.name }, 'moved to ' + t.to) })), { searchLabel: 'Filter transitions' });
  }
  function labelAll() {
    const inp = h('input', { type: 'text', 'aria-label': 'Label', placeholder: 'label' });
    const act = op => () => { const l = inp.value.trim(); if (!l) { inp.focus(); return; } close(); run({ op: 'fields', [op]: [l] }, op === 'labels_add' ? 'labelled ' + l : 'without ' + l); };
    const close = modal(h('div', {}, h('h2', {}, 'Label ' + S.sel.size + ' issues'), inp,
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn', onclick: act('labels_remove') }, 'Remove'), h('button', { class: 'btn primary', onclick: act('labels_add') }, 'Add'))));
  }
  function moveAll() {
    const targets = (S.projects || []).filter(p => !p.mirrored && (p.role === 'member' || p.role === 'admin'));
    A.picker('Move ' + S.sel.size + ' issues to', targets.map(p => ({ label: p.key + ' · ' + p.name, run: () => run({ op: 'move', project: p.key }, 'moved to ' + p.key) })), { searchLabel: 'Filter projects' });
  }

  // ----- row hover actions: assign, mark done, watch, without opening the issue; shown on hover and keyboard focus, only for what
  // the person may do (the issue's own view says so, fetched when the row is first pointed at)
  const views = new Map();
  async function fill(slot) {
    const key = slot.dataset.for;
    if (slot.dataset.filled || !writable(key)) return; // a viewer and a mirrored issue get no buttons, never a failing request
    slot.dataset.filled = '1';
    let v = views.get(key);
    if (!v) { const r = await api('GET', '/api/v1/issues/' + key + '/view'); if (r.status !== 200) return; v = r.data; views.set(key, v); }
    if (v.mirror || !(v.role === 'member' || v.role === 'admin')) return;
    const stop = fn => e => { e.stopPropagation(); e.preventDefault(); fn(); };
    const b = (label, title, fn) => h('button', { class: 'btn ghost hact', 'aria-label': title, title, onclick: stop(fn), onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') e.stopPropagation(); } }, label);
    const kids = [b('👤', 'Assign ' + key, () => A.issueActions.assign(key))];
    if (v.transitions.some(t => t.to_category === 'done')) kids.push(b('✓', 'Mark ' + key + ' done', () => A.issueActions.done(key)));
    const w = await api('GET', '/api/v1/issues/' + key + '/watch');
    const watching = w.status === 200 && w.data.watching;
    const wb = b(watching ? '★' : '☆', (watching ? 'Stop watching ' : 'Watch ') + key, async () => {
      const on = wb.getAttribute('aria-pressed') !== 'true';
      const r = await api('PUT', '/api/v1/issues/' + key + '/watch', { watch: on }); // an explicit choice either way
      if (r.status !== 200) { toast(errText(r)); return; }
      const now = !!r.data.watching;
      wb.setAttribute('aria-pressed', String(now)); wb.textContent = now ? '★' : '☆';
      wb.setAttribute('aria-label', (now ? 'Stop watching ' : 'Watch ') + key); wb.title = wb.getAttribute('aria-label');
      toast(now ? 'Watching ' + key + '.' : 'No longer watching ' + key + '.');
    });
    wb.setAttribute('aria-pressed', String(watching));
    kids.push(wb);
    slot.append(...kids);
  }
  const touch = () => window.matchMedia && window.matchMedia('(hover: none)').matches;
  for (const ev of ['mouseover', 'focusin']) document.addEventListener(ev, e => {
    if (touch()) return; // on phones the row opens on tap; no hover actions (HOVER-03)
    const row = e.target.closest && e.target.closest('#app [role=list] > .row[data-key]');
    if (row) { const slot = row.querySelector('.hover-acts'); if (slot) fill(slot); }
  });
})();
