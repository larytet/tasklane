// The command palette (SPEC 4.6): Ctrl/Cmd+K from anywhere opens one box that jumps to an issue (by key, an old key after a move
// included, or by text), a project, a board or a page, and runs actions. It lists only what the person can see or do: issues come
// from the server's own scoped search, projects and boards from the person's own lists, and actions are shown only to people the
// server would let take them (the server stays the check).
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, api, modal, S } = A;

  const KEY = /^[A-Za-z][A-Za-z0-9]{0,9}-[0-9]{1,9}$/;
  const recentKey = () => 'tl-recent:' + (S.me ? S.me.id : '');
  function recent() { try { return JSON.parse(localStorage.getItem(recentKey()) || '[]'); } catch (e) { return []; } }
  function remember(it) { // the places the person went to, newest first (a convenience of this browser)
    if (!it.hash) return;
    const list = [{ kind: it.kind, label: it.label, hash: it.hash }].concat(recent().filter(x => x.hash !== it.hash)).slice(0, 8);
    try { localStorage.setItem(recentKey(), JSON.stringify(list)); } catch (e) { /* storage unavailable */ }
  }

  function actions() {
    const me = S.me || {}, staff = !!(me.admin || me.org_admin), first = S.projects[0] ? S.projects[0].key : '';
    const go = (label, hash) => ({ kind: 'Page', label, hash });
    const out = [];
    if (S.projects.length) out.push({ kind: 'Action', label: 'Create issue', run: () => A.createDialog() });
    if (staff) out.push({ kind: 'Action', label: 'New project', run: () => A.projectDialog() });
    out.push({ kind: 'Action', label: 'Settings', run: () => A.EXT.settings && A.EXT.settings() });
    if (A.cheatSheet) out.push({ kind: 'Action', label: 'Keyboard shortcuts', run: () => A.cheatSheet() });
    out.push(go('Issues', '#/'), go('Inbox', '#/inbox/unread'), go('Boards', '#/boards/' + first), go('Dashboards', '#/dashboards'), go('Releases', '#/releases'),
      go('Approvals', '#/approvals'), go('Queues', '#/queues'), go('Import', '#/import'), go('Trash', '#/trash/' + first), go('Account and security', '#/account'));
    if (me.admin) for (const [l, x] of [['Workflows', 'workflows'], ['Automation', 'automation'], ['Identity', 'identity'], ['Email', 'email'], ['Git', 'git'], ['Roles', 'roles'], ['Security', 'security'], ['Audit log', 'audit']])
      out.push(go('Admin: ' + l, '#/admin/' + x));
    // the shortcut actions that apply here (close or resolve, assign, ...), from the same registry as the keys
    for (const d of (A.shortcuts || [])) if (d.run && d.palette !== false && (!d.applies || d.applies())) out.push({ kind: 'Shortcut', label: d.label, hint: d.chord || (d.keys || []).join(' then '), run: d.run });
    return out;
  }

  let boards = null; // the boards of the person's projects: the last list is shown at once and fetched again on each opening
  async function loadBoards() {
    const lists = await Promise.all(S.projects.map(p => api('GET', '/api/v1/projects/' + p.key + '/boards').then(r => r.status === 200 ? (r.data.boards || r.data || []).map(b => ({ kind: 'Board', label: b.name + ' · ' + p.key, hash: '#/board/' + b.id })) : [])));
    boards = [].concat(...lists);
    return boards;
  }

  function palette() {
    if (document.querySelector('.scrim[data-palette]')) return;
    const box = h('input', { type: 'text', role: 'combobox', 'aria-label': 'Go to or do', 'aria-expanded': 'true', 'aria-controls': 'pal-list', 'aria-autocomplete': 'list', placeholder: 'Jump to an issue, project, board or page, or run an action', autocomplete: 'off', spellcheck: 'false' });
    const list = h('ul', { class: 'pick', role: 'listbox', id: 'pal-list', 'aria-label': 'Results' });
    const count = h('div', { class: 'sr', role: 'status', 'aria-live': 'polite' });
    let items = [], at = 0, seq = 0, timer = 0;
    const fixed = actions(), projects = S.projects.map(p => ({ kind: 'Project', label: p.key + ' · ' + p.name, hash: '#/?q=' + encodeURIComponent('project = ' + p.key) + '&syntax=tql' }));
    loadBoards().then(() => { if (!box.value) draw(base('')); });
    const match = (it, q) => !q || it.label.toLowerCase().includes(q) || (it.kind || '').toLowerCase() === q;
    function base(q) {
      q = q.toLowerCase();
      const rec = recent().map(r => Object.assign({ recent: true }, r)).filter(r => match(r, q));
      const seen = new Set(rec.map(r => r.hash));
      return rec.concat(fixed.filter(it => match(it, q) && !seen.has(it.hash)), projects.filter(it => match(it, q)), (boards || []).filter(it => match(it, q))).slice(0, q ? 40 : 30);
    }
    function draw(next) {
      items = next; at = Math.min(at, Math.max(0, items.length - 1));
      while (list.firstChild) list.firstChild.remove();
      items.forEach((it, i) => list.append(h('li', { role: 'option', id: 'pal-' + i, 'aria-selected': String(i === at), class: i === at ? 'on' : '', onmousedown: e => { e.preventDefault(); choose(i); } },
        h('span', { class: 'pal-kind' }, it.recent ? 'Recent' : it.kind), ' ', h('span', {}, it.label), it.hint ? h('kbd', { class: 'pal-hint' }, it.hint) : null)));
      if (items[at]) box.setAttribute('aria-activedescendant', 'pal-' + at); else box.removeAttribute('aria-activedescendant');
      if (!items.length) list.append(h('li', { class: 'muted' }, 'Nothing matches'));
      count.textContent = items.length ? items.length + ' results' : 'No results';
    }
    function choose(i) {
      const it = items[i];
      if (!it) return;
      close();
      if (it.hash) { remember(it); location.hash = it.hash; } else it.run();
    }
    async function search(q) {
      const mine = ++seq; // only the answer for the newest text is shown, whatever order the answers come in (PAL-05)
      const local = base(q);
      draw(local);
      if (!q) return;
      const asks = [api('GET', '/api/v1/search/typeahead?q=' + encodeURIComponent(q))];
      if (KEY.test(q)) asks.push(api('GET', '/api/v1/issues/' + encodeURIComponent(q.toUpperCase()) + '/view')); // an exact key, an old one after a move too
      const [ta, exact] = await Promise.all(asks);
      if (mine !== seq || !box.isConnected) return;
      // the exact key first, then the actions and places that match, then issues whose text matches
      const issues = [], exactOnes = [];
      if (exact && exact.status === 200 && !exact.data.issue.archived) { // archived issues are out of the palette, as in the typeahead
        const is = exact.data.issue, old = exact.data.alias && exact.data.alias !== is.key ? ' (was ' + exact.data.alias + ')' : '';
        exactOnes.push({ kind: 'Issue', label: is.key + ' ' + is.summary + old, hash: '#/issue/' + is.key });
      }
      const known = new Set(exactOnes.concat(local).map(x => x.hash));
      if (ta.status === 200) for (const r of ta.data) if (!known.has('#/issue/' + r.key)) { known.add('#/issue/' + r.key); issues.push({ kind: 'Issue', label: r.key + ' ' + r.summary, hash: '#/issue/' + r.key }); }
      // a key typed out goes first even when it was among the recent places
      draw(exactOnes.concat(local.filter(x => !exactOnes.some(e => e.hash === x.hash)), issues));
    }
    box.addEventListener('input', () => { clearTimeout(timer); at = 0; const q = box.value.trim(); draw(base(q)); timer = setTimeout(() => search(q), 150); });
    box.addEventListener('keydown', e => {
      if (e.key === 'ArrowDown') { e.preventDefault(); at = Math.min(items.length - 1, at + 1); draw(items); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); at = Math.max(0, at - 1); draw(items); }
      else if (e.key === 'Enter') { e.preventDefault(); clearTimeout(timer); choose(at); }
    });
    const close = modal(h('div', { class: 'palette' }, h('h2', { class: 'sr' }, 'Command palette'), box, list, count));
    close.scrim.dataset.palette = '1';
    draw(base(''));
    box.focus();
  }
  A.palette = palette;
  // Ctrl+K and Cmd+K, from anywhere, a text field included; the browser's own Ctrl+K (Firefox's search bar) is not triggered
  document.addEventListener('keydown', e => {
    if (!S.me || e.repeat || !(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey || (e.key || '').toLowerCase() !== 'k') return;
    e.preventDefault();
    const open = document.querySelector('.scrim[data-palette]');
    if (open) open.querySelector('input').focus(); else palette();
  }, true);
  if (A.registerShortcut) A.registerShortcut({ chord: 'Ctrl+K', screen: 'Anywhere', label: 'Command palette: go to or do anything', palette: false });
})();
