// The Settings dialog (SPEC 4.6): the person's own settings in one place, saved on the server as each one changes and applied at once,
// without a reload. Two-factor authentication, passkeys, the password, sessions and API tokens are here too (tabs Security, Sessions and
// API tokens, built by account.js); Settings (the gear in the top bar) has them on Security.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, S, saveSettings, api } = A;

  function settingsDialog(initial) { // initial: the tab to open on, as in the address (security, api-tokens)
    const status = h('div', { class: 'saved muted', role: 'status' });
    const save = async ch => {
      status.textContent = 'Saving…';
      const ok = await saveSettings(ch);
      status.textContent = ok ? 'Saved' : 'Could not save the settings';
      status.className = 'saved ' + (ok ? 'muted' : 'err');
      return ok;
    };
    let n = 0;
    const row = (label, ctl) => { const id = ctl.id || (ctl.id = 'set-' + (++n)); return [h('label', { for: id }, label), ctl]; };
    const radios = (label, key, opts) => {
      const g = h('div', { role: 'radiogroup', 'aria-label': label });
      for (const [v, text] of opts) {
        const r = h('input', { type: 'radio', name: 'set-' + key, value: v, checked: String(S.settings[key]) === String(v) });
        r.addEventListener('change', () => { if (r.checked) save({ [key]: typeof S.settings[key] === 'number' ? Number(v) : v }); });
        g.append(h('label', { class: 'opt' }, r, text));
      }
      return [h('span', { id: 'set-l-' + key }, label), g];
    };
    const check = (label, key) => {
      const c = h('input', { type: 'checkbox', checked: !!S.settings[key] });
      c.addEventListener('change', () => save({ [key]: c.checked }));
      return row(label, c);
    };
    const select = (label, key, opts) => {
      const s = h('select', {}, opts.map(([v, t]) => h('option', { value: v, selected: String(S.settings[key]) === String(v) }, t)));
      s.addEventListener('change', () => save({ [key]: typeof S.settings[key] === 'number' ? Number(s.value) : s.value }));
      return row(label, s);
    };
    const chars = h('input', { type: 'number', min: 1, max: 5000, value: S.settings.snippet_chars, 'aria-label': 'Description length in the long view' });
    chars.addEventListener('change', async () => { if (!(await save({ snippet_chars: Number(chars.value) }))) chars.value = S.settings.snippet_chars; });

    // the list's columns: shown or not, and their order; the key and the summary always show (LIST-02)
    const BUILTIN = ['type', 'key', 'summary', 'priority', 'assignee', 'status', 'updated', 'created', 'reporter', 'labels', 'comments'];
    const LABEL = { type: 'Type', key: 'Key', summary: 'Summary', priority: 'Priority', assignee: 'Assignee', status: 'Status', updated: 'Updated', created: 'Created', reporter: 'Reporter', labels: 'Labels', comments: 'Comments' };
    const fnames = Object.assign({}, A.S.fieldNames || {});
    const columnsBox = h('ul', { class: 'colpick', 'aria-labelledby': 'set-l-cols' });
    let order = [];
    const drawCols = () => {
      const chosen = S.settings.columns || [];
      const all = chosen.concat(BUILTIN.filter(c => !chosen.includes(c)), Object.keys(fnames).filter(c => !chosen.includes(c)));
      order = all;
      while (columnsBox.firstChild) columnsBox.firstChild.remove();
      all.forEach((c, i) => {
        const fixed = c === 'key' || c === 'summary', on = fixed || chosen.includes(c), label = LABEL[c] || fnames[c] || c;
        const cb = h('input', { type: 'checkbox', checked: on, disabled: fixed, 'aria-label': 'Show ' + label, title: fixed ? 'Always shown' : '' });
        cb.addEventListener('change', () => saveCols(order.filter(x => x === c ? cb.checked : (x === 'key' || x === 'summary' || (S.settings.columns || []).includes(x)))));
        const move = d => () => { const cur = (S.settings.columns || []).slice(), at = cur.indexOf(c); if (at < 0 || at + d < 0 || at + d >= cur.length) return; cur.splice(at, 1); cur.splice(at + d, 0, c); saveCols(cur, c, d); };
        columnsBox.append(h('li', {}, cb, h('span', { class: 'nm' }, label),
          on ? h('button', { class: 'btn ghost', 'aria-label': 'Move ' + label + ' earlier', disabled: i === 0, onclick: move(-1) }, '↑') : null,
          on ? h('button', { class: 'btn ghost', 'aria-label': 'Move ' + label + ' later', disabled: i >= chosen.length - 1, onclick: move(1) }, '↓') : null));
      });
    };
    const saveCols = async (cols, moved, d) => {
      await save({ columns: cols.filter(c => c === 'key' || c === 'summary' || cols.includes(c)) });
      drawCols();
      if (moved) { const b = columnsBox.querySelector('[aria-label="Move ' + (LABEL[moved] || fnames[moved] || moved) + (d < 0 ? ' earlier' : ' later') + '"]'); if (b && !b.disabled) b.focus(); }
    };
    drawCols();
    if (!A.S.fieldNames) api('GET', '/api/v1/fields').then(r => { if (r.status === 200) { for (const f of r.data) if (!f.trashed) fnames[f.key || 'customfield_' + f.id] = f.name; drawCols(); fillSort(); } });
    const SORTS = ['key', 'summary', 'priority', 'assignee', 'status', 'updated', 'created', 'reporter', 'type'];
    const sortField = h('select', {}), sortDir = h('select', {}, h('option', { value: 'asc' }, 'Ascending'), h('option', { value: 'desc' }, 'Descending'));
    const fillSort = () => {
      while (sortField.firstChild) sortField.firstChild.remove();
      sortField.append(h('option', { value: '' }, "The query's own order"), SORTS.map(c => h('option', { value: c }, LABEL[c])), Object.keys(fnames).map(c => h('option', { value: c }, fnames[c])));
      const so = S.settings.sort || {}; sortField.value = so.field || ''; sortDir.value = so.dir || 'asc'; sortDir.disabled = !so.field;
    };
    fillSort();
    const saveSort = async () => { await save({ sort: { field: sortField.value, dir: sortDir.value } }); sortDir.disabled = !sortField.value };
    sortField.addEventListener('change', saveSort); sortDir.addEventListener('change', saveSort);
    // Jira (SPEC 18.1.1): the projects that save to Jira as the person need their own Jira API token. It is kept in memory only; here it is
    // set before the first save, replaced, or forgotten, and the link says where the token is created for that Jira site.
    const jiraList = h('div', { class: 'list', role: 'list', 'aria-label': 'Jira connections that need your token' });
    const drawJira = async () => {
      const r = await api('GET', '/api/v1/jira/session-token');
      while (jiraList.firstChild) jiraList.firstChild.remove();
      if (r.status !== 200) { jiraList.append(h('div', { class: 'row muted', role: 'listitem' }, 'Could not read your Jira tokens.')); return 0; }
      for (const c of r.data) {
        const detail = { connection_id: c.connection_id, connection: c.connection, site_url: c.site_url, token_page: c.token_page };
        jiraList.append(h('div', { class: 'row', role: 'listitem' },
          h('span', { class: 'sum' }, h('strong', {}, c.connection), ' ', h('span', { class: 'muted' }, c.site_url + ' · ' + c.projects.join(', '))),
          h('span', { class: c.has_token ? '' : 'err', role: 'status' }, c.has_token ? 'Token set for this session' : 'No token: you are asked at your first save'),
          h('button', { class: 'btn', 'aria-label': (c.has_token ? 'Replace' : 'Set') + ' your token for ' + c.connection, onclick: async () => { if (await A.askJiraToken(detail)) drawJira(); } }, c.has_token ? 'Replace token…' : 'Set token…'),
          c.has_token ? h('button', { class: 'btn ghost', 'aria-label': 'Forget your token for ' + c.connection, onclick: async () => { await api('DELETE', '/api/v1/jira/session-token?connection=' + encodeURIComponent(c.connection_id)); drawJira(); } }, 'Forget') : null,
          h('a', { href: c.token_page, target: '_blank', rel: 'noopener noreferrer', 'aria-label': 'Where to create a token for ' + c.connection }, 'Create a token')));
      }
      return r.data.length;
    };
    const jiraPanel = h('div', {}, h('p', { class: 'muted', style: 'margin-top:0' }, 'Some projects save your changes to Jira as you, and for those Tasklane needs your personal Jira API token. The token is kept only in memory for this session: it is never written to disk or to the database, and it is forgotten when you sign out or when Tasklane restarts.'), jiraList);
    const accountPanels = A.EXT.accountSections ? A.EXT.accountSections() : {};
    const panels = {
      Appearance: h('div', { class: 'set' },
        radios('Theme', 'theme', [['', 'This device'], ['dark', 'Dark'], ['light', 'Light']]),
        select('Font size', 'font_scale', [[90, '90%'], [100, '100%'], [115, '115%'], [130, '130%']]),
        check('High contrast', 'high_contrast'),
        radios('Reduce motion', 'reduced_motion', [['system', 'Follow the system'], ['on', 'On'], ['off', 'Off']])),
      List: h('div', { class: 'set' },
        radios('List view', 'list_view', [['short', 'Short'], ['long', 'Long']]),
        radios('Density', 'density', [['compact', 'Compact'], ['default', 'Default'], ['comfortable', 'Comfortable']]),
        radios('Page width', 'page_width', [['wide', 'Wide'], ['narrow', 'Narrow']]),
        row('Characters of the description shown in the long view (1 to 5000)', chars),
        h('span', { id: 'set-l-cols' }, 'Columns'), columnsBox,
        row('Sort by', sortField), row('Direction', sortDir)),
      Keyboard: h('div', { class: 'set' },
        check('Single-key shortcuts', 'shortcuts'),
        h('span', {}), h('p', { class: 'muted', style: 'margin:0' }, 'Shortcuts such as c to create and j/k to move. Ctrl+K and Ctrl+Enter work either way.')),
      ...accountPanels,
      Jira: jiraPanel,
    };
    // the instance administrator's screens: the tab shows their own row of tabs
    if (S.me && S.me.admin && A.EXT.adminTabs) panels.Administration = h('div', {}, A.EXT.adminTabs(''));
    const tabs = h('div', { role: 'tablist', 'aria-label': 'Settings sections' });
    const box = h('div', {});
    const names = Object.keys(panels);
    const select_ = (name, focus) => {
      for (const t of tabs.children) { const on = t.dataset.name === name; t.setAttribute('aria-selected', String(on)); t.tabIndex = on ? 0 : -1; if (on && focus) t.focus(); }
      for (const [k, p] of Object.entries(panels)) p.hidden = k !== name;
    };
    names.forEach((name, i) => {
      const t = h('button', { role: 'tab', id: 'set-tab-' + i, 'aria-controls': 'set-panel-' + i, 'data-name': name, onclick: () => { select_(name); history.replaceState(null, '', '#/settings/' + slug(name)); } }, name);
      t.addEventListener('keydown', e => { // arrow keys move between the tabs (the tab pattern); Tab goes into the panel
        const d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : e.key === 'Home' ? -i : e.key === 'End' ? names.length - 1 - i : 0;
        if (d) { e.preventDefault(); const to = names[(i + d + names.length) % names.length]; select_(to, true); history.replaceState(null, '', '#/settings/' + slug(to)); }
      });
      tabs.append(t);
      Object.assign(panels[name], { id: 'set-panel-' + i, role: 'tabpanel' }); panels[name].setAttribute('aria-labelledby', 'set-tab-' + i);
      box.append(panels[name]);
    });
    select_(names.find(n => slug(n) === initial) || names[0]);
    A.S.settingsNames = names;
    // the Jira tab exists only for a person who has a project that saves to Jira
    drawJira().then(n => {
      if (n) return;
      const t = tabs.querySelector('[data-name="Jira"]'); if (t) t.remove();
      jiraPanel.remove(); delete panels.Jira; names.splice(names.indexOf('Jira'), 1); A.S.settingsNames = names;
    });
    const done = h('button', { class: 'btn primary', onclick: () => { location.hash = A.S.settingsFrom && !/^#\/(settings|account)/.test(A.S.settingsFrom) ? A.S.settingsFrom : '#/'; } }, 'Done');
    const app = A.clear(document.getElementById('app'));
    app.append(A.topbar(), h('div', { class: 'page settings', id: 'settings-page', role: 'region', 'aria-label': 'Settings' }, h('h2', {}, 'Settings'), tabs, box, status,
      h('div', { class: 'actions' }, h('span', { class: 'grow' }), done)));
    // a change made in another tab or by the view buttons shows here too
    const sync = () => { chars.value = S.settings.snippet_chars; };
    A.P.listeners.push(sync);
    // Escape goes back, as it closed the dialog (not while a dialog opened from here is up, nor while a control keeps the key)
    const esc = e => {
      if (!document.getElementById('settings-page')) return document.removeEventListener('keydown', esc);
      if (e.key !== 'Escape' || e.defaultPrevented || document.querySelector('.scrim')) return;
      if (e.target && e.target.closest && e.target.closest('select, [aria-expanded=true]')) return;
      done.click();
    };
    document.addEventListener('keydown', esc);
    const obs = new MutationObserver(() => { if (!document.getElementById('settings-page')) { A.P.listeners.splice(A.P.listeners.indexOf(sync), 1); obs.disconnect(); } });
    obs.observe(document.getElementById('app'), { childList: true });
  }
  // Settings is a screen of its own (#/settings/<tab>), like the administration pages, not a dialog
  const slug = name => name.toLowerCase().replace(/[^a-z0-9]+/g, '-');
  function openSettings(tab) { // as a click handler it gets the event: only a name is a tab
    if (!/^#\/(settings|account)/.test(location.hash)) A.S.settingsFrom = location.hash || '#/';
    location.hash = '#/settings' + (typeof tab === 'string' && tab ? '/' + slug(tab) : '');
  }
  A.EXT.routes.push({ re: /^#\/settings(?:\/([a-z0-9-]+))?\/?$/, run: m => {
    const want = m[1] || '';
    // the tab is chosen by its address; the dialog's own tab names are matched without case
    settingsDialog(want);
  } });
  A.EXT.settings = openSettings;
  // the same row of tabs on an administration screen, as links back into Settings; Administration is the one selected
  A.EXT.settingsRow = () => h('nav', { class: 'tabs', 'aria-label': 'Settings sections' },
    ...(A.S.settingsNames || ['Appearance', 'List', 'Keyboard', 'Security', 'Sessions', 'API tokens', 'Administration']).map(n =>
      h('a', { href: '#/settings/' + slug(n), class: 'tab', 'aria-current': n === 'Administration' ? 'page' : null }, n)));
})();
