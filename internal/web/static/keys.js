// Keyboard shortcuts (SPEC 4.6, 20): one registry that the key handler and the "?" cheat sheet are both built from, so the sheet never
// drifts from what the keys do. Single-key shortcuts are inert while typing in a field or while a dialog is open, and can be turned
// off in Settings (WCAG 2.2 SC 2.1.4); chords with Ctrl or Cmd always work.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, api, toast, modal, S } = A;

  // keys: one key, or two pressed one after the other (g then i); screen: where it applies, as the cheat sheet groups them
  const REG = [];
  function register(def) {
    if (!def.label) throw new Error('shortcut ' + (def.keys || [def.chord]).join(' ') + ' has no label'); // KEY-10: nothing reaches the sheet unnamed
    def.applies = () => applies(def); // the palette offers the actions that apply on this screen
    REG.push(def);
  }
  A.shortcuts = REG;
  // the row hover actions and the bulk bar use the same actions and the same chooser
  A.issueActions = { done: k => closeOrResolve(k), assign: k => assign(k) };
  A.registerShortcut = register;

  const screen = () => {
    const hs = location.hash || '#/';
    if (hs.startsWith('#/issue/')) return 'Issue';
    if (hs === '#/' || hs === '#' || hs.startsWith('#/?')) return 'Issue list';
    return 'Other';
  };
  // a field that takes text; a checkbox or a radio button with focus does not (x and j still work from a row's checkbox)
  const typing = t => t && (t.isContentEditable || /^(TEXTAREA|SELECT)$/.test(t.tagName) || (t.tagName === 'INPUT' && !/^(checkbox|radio|button|submit)$/.test(t.type)));
  const dialogOpen = () => !!document.querySelector('.scrim');

  // a polite live region: the current row and the outcome of an action are said once, not on every repaint (KEY-11)
  let live = null;
  function say(text) {
    if (!live) { live = h('div', { class: 'sr', role: 'status', 'aria-live': 'polite', id: 'kbd-live' }); document.body.append(live); }
    live.textContent = '';
    setTimeout(() => { live.textContent = text; }, 30);
  }

  // ----- the current row of the issue list
  const rows = () => [...document.querySelectorAll('#app [role=list] > .row[data-key]')];
  function setCurrent(i) {
    const rs = rows();
    if (!rs.length) return null;
    i = Math.max(0, Math.min(rs.length - 1, i)); // j on the last row and k on the first stay put
    rs.forEach((r, k) => { r.classList.toggle('current', k === i); if (k === i) r.setAttribute('aria-current', 'true'); else r.removeAttribute('aria-current'); });
    const r = rs[i];
    if (S.cur !== i) {
      S.cur = i;
      r.scrollIntoView({ block: 'nearest' });
      r.focus({ preventScroll: true });
      say(r.dataset.key + ' ' + ((r.querySelector('.sum') || {}).textContent || '') + ', ' + (i + 1) + ' of ' + rs.length);
    }
    return r;
  }
  const currentRow = () => { const rs = rows(); return rs[S.cur] || null; };
  // the issue an action is for: the open issue, or the current row of the list
  function targetKey() {
    if (screen() === 'Issue' && S.view && S.view.v) return S.view.v.issue.key;
    const r = currentRow();
    return r ? r.dataset.key : null;
  }
  const refresh = () => A.route();

  async function viewOf(key) {
    const r = await api('GET', '/api/v1/issues/' + key + '/view');
    return r.status === 200 ? r.data : null;
  }
  const canWrite = v => !v.mirror && (v.role === 'member' || v.role === 'admin');
  const whyNot = v => v.mirror ? 'This issue is mirrored from Jira and is changed there.' : 'You can only view ' + v.issue.key + ', so you cannot change it.';

  // a short list to choose from: arrows, Enter, Escape; typing filters when there is a search box
  function picker(title, items, opts) {
    opts = opts || {};
    let shown = items, at = 0;
    const box = h('input', { type: 'search', 'aria-label': opts.searchLabel || 'Search', placeholder: opts.placeholder || '', role: 'combobox', 'aria-expanded': 'true', 'aria-controls': 'pick-list', 'aria-autocomplete': 'list' });
    const list = h('ul', { class: 'pick', role: 'listbox', id: 'pick-list', 'aria-label': title });
    const draw = () => {
      while (list.firstChild) list.firstChild.remove();
      shown.forEach((it, i) => list.append(h('li', { role: 'option', id: 'pick-' + i, 'aria-selected': String(i === at), class: i === at ? 'on' : '', onmousedown: e => { e.preventDefault(); choose(i); } }, it.label)));
      if (shown[at]) box.setAttribute('aria-activedescendant', 'pick-' + at); else box.removeAttribute('aria-activedescendant');
      if (!shown.length) list.append(h('li', { class: 'muted' }, 'Nothing matches'));
    };
    const choose = i => { const it = shown[i]; if (!it) return; close(); it.run(); };
    box.addEventListener('input', () => { const q = box.value.trim().toLowerCase(); shown = items.filter(it => !q || it.always || it.label.toLowerCase().includes(q)); at = 0; draw(); });
    box.addEventListener('keydown', e => {
      if (e.key === 'ArrowDown') { e.preventDefault(); at = Math.min(shown.length - 1, at + 1); draw(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); at = Math.max(0, at - 1); draw(); }
      else if (e.key === 'Enter') { e.preventDefault(); choose(at); }
    });
    const close = modal(h('div', { class: 'picker' }, h('h2', {}, title), box, list));
    draw();
    box.focus();
    return close;
  }

  A.picker = (title, items, opts) => picker(title, items, opts);

  // ----- e: close or resolve (the transition into the done category; a choice when there are several)
  async function closeOrResolve(forKey) {
    const key = typeof forKey === 'string' ? forKey : targetKey();
    if (!key) return;
    const v = await viewOf(key);
    if (!v) { toast('That issue could not be loaded.'); return; }
    if (!canWrite(v)) { toast(whyNot(v)); say(whyNot(v)); return; }
    const done = v.transitions.filter(t => t.to_category === 'done');
    const run = async t => {
      const r = await api('POST', '/api/v1/issues/' + key + '/transitions', { transition: t.id, base_version: v.issue.version }, A.WANT_UNDO);
      if (r.status === 200) { A.undoToast(key + ' moved to ' + t.to + '.', r, refresh); refresh(); }
      else toast((r.data && r.data.error) || 'The transition was refused.');
    };
    if (!done.length) { const m = key + ' has no transition to a done status from ' + v.issue.status + '.'; toast(m); say(m); return; }
    if (done.length === 1) return run(done[0]);
    picker('Close or resolve ' + key, done.map(t => ({ label: t.name + ' → ' + t.to, run: () => run(t) })), { searchLabel: 'Filter transitions' });
  }

  // ----- a: assign (me first, then Unassigned, then people)
  async function assign(forKey) {
    const key = typeof forKey === 'string' ? forKey : targetKey();
    if (!key) return;
    const v = await viewOf(key);
    if (!v) { toast('That issue could not be loaded.'); return; }
    if (!canWrite(v)) { toast(whyNot(v)); say(whyNot(v)); return; }
    const set = async (id, name) => {
      // the version the person saw: if someone changed the issue since, the server says so instead of overwriting (KEY-06)
      const r = await api('PATCH', '/api/v1/issues/' + key, { assignee: id }, { 'If-Match': '"' + v.issue.version + '"' });
      if (r.status === 200) { toast(key + (id ? ' assigned to ' + name + '.' : ' is unassigned.')); say(key + (id ? ' assigned to ' + name : ' unassigned')); refresh(); }
      else if (r.status === 412 || r.status === 409) { toast(key + ' was changed by someone else just now; nothing was overwritten. Look again and retry.'); refresh(); }
      else toast((r.data && r.data.error) || 'Could not assign.');
    };
    const me = S.me;
    const people = (S.users || []).filter(u => u.id !== me.id).sort((a, b) => a.name.localeCompare(b.name));
    picker('Assign ' + key, [{ label: 'Me (' + (me.label || me.name || me.email) + ')', always: true, run: () => set(me.id, 'you') }, { label: 'Unassigned', always: true, run: () => set('', '') }]
      .concat(people.map(u => ({ label: u.name, run: () => set(u.id, u.name) }))), { searchLabel: 'Search people', placeholder: 'Type a name' });
  }

  // ----- the cheat sheet, from the same table
  function cheatSheet() {
    const groups = {};
    for (const d of REG) (groups[d.screen] = groups[d.screen] || []).push(d);
    const kbd = d => d.chord ? h('kbd', {}, d.chord) : d.keys.map((k, i) => [i ? h('span', { class: 'muted' }, ' then ') : null, h('kbd', {}, k)]);
    const tables = Object.entries(groups).map(([name, ds]) => h('section', {}, h('h3', {}, name),
      h('table', { class: 'data keys' }, h('tbody', {}, ds.map(d => h('tr', {}, h('th', { scope: 'row' }, kbd(d)), h('td', {}, d.label)))))));
    const note = S.settings && S.settings.shortcuts === false ? h('p', { class: 'note' }, 'Single-key shortcuts are turned off in Settings; Ctrl and Cmd shortcuts still work.') : null;
    const close = modal(h('div', { class: 'cheats' }, h('h2', {}, 'Keyboard shortcuts'), note, tables, h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Close'))));
    close.scrim.dataset.cheats = '1';
  }

  const go = hash => () => { location.hash = hash; };
  register({ keys: ['c'], screen: 'Anywhere', label: 'Create an issue', run: () => { if (S.projects.length) A.createDialog(); } });
  register({ keys: ['/'], screen: 'Anywhere', label: 'Search', run: () => { const q = document.querySelector('.bar input[type=search]'); if (q) { q.focus(); q.select(); } } });
  register({ keys: ['g', 'i'], screen: 'Anywhere', label: 'Go to the inbox', run: go('#/inbox/unread') });
  register({ keys: ['g', 'b'], screen: 'Anywhere', label: 'Go to boards', run: () => { location.hash = '#/boards/' + (S.projects[0] ? S.projects[0].key : ''); } });
  register({ keys: ['?'], screen: 'Anywhere', label: 'Show these shortcuts', run: cheatSheet, palette: false }); // the palette has it as an action
  A.cheatSheet = cheatSheet;
  // past the last row of a page j goes on to the next page, and k before the first back to the previous one (LIST-05)
  register({ keys: ['j'], screen: 'Issue list', label: 'Next issue', run: async () => {
    if (S.cur >= 0 && S.cur === rows().length - 1 && A.EXT.nextPage && await A.EXT.nextPage()) { S.cur = -1; setCurrent(0); return; }
    setCurrent(S.cur < 0 ? 0 : S.cur + 1);
  } });
  register({ keys: ['k'], screen: 'Issue list', label: 'Previous issue', run: async () => {
    if (S.cur === 0 && A.EXT.prevPage && await A.EXT.prevPage()) { S.cur = -1; setCurrent(rows().length - 1); return; }
    setCurrent(S.cur < 0 ? 0 : S.cur - 1);
  } });
  register({ keys: ['o'], screen: 'Issue list', label: 'Open the current issue (or Enter)', run: () => { const r = currentRow(); if (r) location.hash = '#/issue/' + r.dataset.key; } });
  register({ keys: ['x'], screen: 'Issue list', label: 'Select the current issue', run: () => {
    const r = currentRow() || setCurrent(0);
    if (!r) return;
    const c = r.querySelector('input.rowsel');
    c.checked = !c.checked; c.dispatchEvent(new Event('change'));
    say((c.checked ? 'Selected ' : 'Not selected ') + r.dataset.key + ', ' + S.sel.size + ' selected');
  } });
  register({ keys: ['e'], screen: 'Issue list and issue', label: 'Close or resolve', run: closeOrResolve });
  register({ keys: ['a'], screen: 'Issue list and issue', label: 'Assign', run: assign });
  register({ keys: ['j'], screen: 'Issue', label: 'Next issue of the list you came from', run: () => navIssue(1) });
  register({ keys: ['k'], screen: 'Issue', label: 'Previous issue of the list you came from', run: () => navIssue(-1) });
  register({ chord: 'Ctrl+Enter', screen: 'While writing', label: 'Send the comment, or save the description being edited', native: true });

  function navIssue(d) {
    const key = targetKey(), keys = (S.nav && S.nav.keys) || [];
    const i = keys.indexOf(key);
    if (i < 0) return; // opened from a link, with no list behind it: nothing to walk
    const to = keys[i + d];
    if (to) location.hash = '#/issue/' + to;
  }

  // which entries apply on this screen
  const applies = d => d.screen === 'Anywhere' || d.screen === screen() || (d.screen === 'Issue list and issue' && (screen() === 'Issue list' || screen() === 'Issue'));

  let pending = null, pendingTimer = 0; // the first key of a sequence (g), forgotten after a second
  document.addEventListener('keydown', e => {
    if (!S.me || e.defaultPrevented) return;
    if (e.ctrlKey || e.metaKey || e.altKey) return; // browser and system shortcuts are never swallowed (KEY-04)
    if (typing(e.target)) return;
    if (dialogOpen()) {
      // the sheet closes with ? as well as Escape; every other dialog keeps its keys
      const sc = document.querySelector('.scrim[data-cheats]');
      if (sc && e.key === '?') { e.preventDefault(); sc.querySelector('.actions .btn').click(); }
      return;
    }
    if (S.settings && S.settings.shortcuts === false) return; // turned off (KEY-09)
    if (e.key === 'Enter' && screen() === 'Issue list') { // Enter opens the current row (a focused row also opens itself)
      const r = currentRow();
      if (r && document.activeElement !== r) { e.preventDefault(); location.hash = '#/issue/' + r.dataset.key; }
      return;
    }
    // the typed character, not the physical key, so other keyboard layouts and Caps Lock follow it (KEY-04)
    const ch = e.key.length === 1 ? e.key.toLowerCase() : null;
    if (!ch) return;
    if (pending) {
      const first = pending; pending = null; clearTimeout(pendingTimer);
      const d = REG.find(x => x.keys && x.keys.length === 2 && x.keys[0] === first && x.keys[1] === ch && applies(x));
      if (d) { e.preventDefault(); d.run(); }
      return; // an unknown second key ends the sequence and does nothing (KEY-03)
    }
    if (REG.some(x => x.keys && x.keys.length === 2 && x.keys[0] === ch && applies(x))) {
      e.preventDefault(); pending = ch; pendingTimer = setTimeout(() => { pending = null; }, 1000); return;
    }
    const d = REG.find(x => x.keys && x.keys.length === 1 && x.keys[0] === ch && applies(x));
    if (d) { e.preventDefault(); d.run(); }
  });
  // Ctrl/Cmd+Enter in any other field being edited (the summary, a custom field) saves it by leaving it
  document.addEventListener('keydown', e => {
    if (e.key !== 'Enter' || !(e.ctrlKey || e.metaKey) || e.defaultPrevented || e.repeat) return;
    const t = e.target;
    if (t && t.tagName === 'INPUT' && t.type !== 'checkbox') { e.preventDefault(); t.blur(); }
  });
  // a list row with focus is the current row, so the mouse and the keys agree
  document.addEventListener('focusin', e => {
    const r = e.target.closest && e.target.closest('#app [role=list] > .row[data-key]');
    if (r) { const i = rows().indexOf(r); if (i >= 0 && i !== S.cur) { S.cur = i; rows().forEach((x, k) => { x.classList.toggle('current', k === i); if (k === i) x.setAttribute('aria-current', 'true'); else x.removeAttribute('aria-current'); }); } }
  });
})();
