// The Settings dialog (SPEC 4.6): the person's own settings in one place, saved on the server as each one changes and applied at once,
// without a reload. Security, sessions and two-factor stay on the Account page.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, modal, S, saveSettings } = A;

  function settingsDialog() {
    const status = h('div', { class: 'saved muted', role: 'status' });
    let redraw = false; // the list's rows depend on these: the screen is drawn again when the dialog closes
    const save = async ch => {
      status.textContent = 'Saving…';
      const ok = await saveSettings(ch);
      if (ok && ('list_view' in ch || 'snippet_chars' in ch)) redraw = true;
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
        row('Characters of the description shown in the long view (1 to 5000)', chars)),
      Keyboard: h('div', { class: 'set' },
        check('Single-key shortcuts', 'shortcuts'),
        h('span', {}), h('p', { class: 'muted', style: 'margin:0' }, 'Shortcuts such as c to create and j/k to move. Ctrl+K and Ctrl+Enter work either way.')),
    };
    const tabs = h('div', { role: 'tablist', 'aria-label': 'Settings sections' });
    const box = h('div', {});
    const names = Object.keys(panels);
    const select_ = (name, focus) => {
      for (const t of tabs.children) { const on = t.dataset.name === name; t.setAttribute('aria-selected', String(on)); t.tabIndex = on ? 0 : -1; if (on && focus) t.focus(); }
      for (const [k, p] of Object.entries(panels)) p.hidden = k !== name;
    };
    names.forEach((name, i) => {
      const t = h('button', { role: 'tab', id: 'set-tab-' + i, 'aria-controls': 'set-panel-' + i, 'data-name': name, onclick: () => select_(name) }, name);
      t.addEventListener('keydown', e => { // arrow keys move between the tabs (the tab pattern); Tab goes into the panel
        const d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : e.key === 'Home' ? -i : e.key === 'End' ? names.length - 1 - i : 0;
        if (d) { e.preventDefault(); select_(names[(i + d + names.length) % names.length], true); }
      });
      tabs.append(t);
      Object.assign(panels[name], { id: 'set-panel-' + i, role: 'tabpanel' }); panels[name].setAttribute('aria-labelledby', 'set-tab-' + i);
      box.append(panels[name]);
    });
    select_(names[0]);
    const close = modal(h('div', { class: 'settings' }, h('h2', {}, 'Settings'), tabs, box, status,
      h('div', { class: 'actions' }, h('a', { href: '#/account', onclick: () => close() }, 'Security, sessions and two-factor'), h('span', { class: 'grow' }), h('button', { class: 'btn primary', onclick: () => close() }, 'Done'))));
    close.scrim.querySelector('.dialog').classList.add('settings');
    // a change made in another tab or by the view buttons shows here too
    const sync = () => { chars.value = S.settings.snippet_chars; };
    A.P.listeners.push(sync);
    const obs = new MutationObserver(() => { if (!close.scrim.isConnected) { A.P.listeners.splice(A.P.listeners.indexOf(sync), 1); obs.disconnect(); if (redraw) A.route(); } });
    obs.observe(document.body, { childList: true });
  }
  A.EXT.settings = settingsDialog;
})();
