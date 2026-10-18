// Releases: a project's versions with progress, release and unrelease (with a warning about unfinished work), and release notes.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, modal, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';

  function progress(p) {
    if (!p || !p.total) return h('span', { class: 'muted' }, 'No issues');
    const seg = (n, cls, label) => n ? h('span', { class: 'rel-seg ' + cls, style: 'width:' + (100 * n / p.total) + '%', title: label + ': ' + n }) : null;
    return h('div', { class: 'rel-progress' },
      h('span', { class: 'rel-bar', role: 'img', 'aria-label': p.done + ' of ' + p.total + ' done, ' + p.in_progress + ' in progress, ' + p.todo + ' to do' }, seg(p.done, 'done', 'Done'), seg(p.in_progress, 'prog', 'In progress'), seg(p.todo, 'todo', 'To do')),
      h('span', { class: 'muted' }, p.done + ' of ' + p.total + ' done'));
  }

  async function releasesView(key) {
    const app = clear(document.getElementById('app'));
    key = key.toUpperCase();
    const sel = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === key }, p.key + ' · ' + p.name)));
    sel.addEventListener('change', () => { location.hash = '#/releases/' + sel.value; });
    const list = h('div', { class: 'list', role: 'list', 'aria-label': 'Versions' });
    const msg = h('div', { class: 'err', role: 'alert' });
    const showArchived = h('input', { type: 'checkbox', id: 'rel-arch' });
    let canManage = false;
    const form = h('div', { class: 'chips', hidden: true });

    async function load() {
      clear(list);
      const r = await api('GET', '/api/v1/projects/' + key + '/versions' + (showArchived.checked ? '?archived=1' : ''));
      if (r.status !== 200) { list.append(h('div', { class: 'row', role: 'listitem' }, r.status === 404 ? 'That project does not exist, or you cannot see it.' : errText(r))); return; }
      canManage = r.data.can_manage;
      form.hidden = !canManage;
      if (!r.data.versions.length) list.append(h('div', { class: 'row', role: 'listitem' }, h('span', { class: 'muted' }, 'No versions yet.')));
      for (const v of r.data.versions) list.append(row(v, r.data.versions));
    }

    function row(v, all) {
      const dates = [v.start_date ? 'from ' + v.start_date : '', v.release_date ? (v.released ? 'released ' : 'due ') + v.release_date : ''].filter(Boolean).join(' · ');
      const act = [];
      act.push(h('button', { class: 'btn ghost', onclick: () => notes(v) }, 'Notes'));
      act.push(h('a', { class: 'btn ghost', href: '#/?q=' + encodeURIComponent('fixVersion = "' + v.name + '" AND project = ' + key), 'aria-label': 'Issues in ' + v.name }, 'Issues'));
      if (v.can_manage) {
        act.push(v.released ? h('button', { class: 'btn ghost', 'aria-label': 'Unrelease ' + v.name, onclick: async () => { const x = await api('POST', '/api/v1/versions/' + v.id + '/unrelease'); if (x.status === 200) load(); else toast(errText(x)); } }, 'Unrelease')
          : h('button', { class: 'btn', 'aria-label': 'Release ' + v.name, onclick: () => release(v, all) }, 'Release'));
        act.push(h('button', { class: 'btn ghost', 'aria-label': (v.archived ? 'Unarchive ' : 'Archive ') + v.name, onclick: async () => { const x = await api('PUT', '/api/v1/versions/' + v.id, { archived: !v.archived }); if (x.status === 200) load(); else toast(errText(x)); } }, v.archived ? 'Unarchive' : 'Archive'));
        act.push(h('button', { class: 'btn ghost', 'aria-label': 'Delete ' + v.name, onclick: () => del(v, all) }, 'Delete'));
      }
      return h('div', { class: 'row rel-row', role: 'listitem' },
        h('span', { class: 'sum' }, h('strong', {}, v.name), ' ', v.released ? h('span', { class: 'badge' }, 'Released') : null, v.overdue ? h('span', { class: 'badge', style: 'background:var(--danger,#c0392b);color:#fff' }, 'Overdue') : null, v.archived ? h('span', { class: 'muted' }, ' archived') : null,
          h('div', { class: 'muted' }, [dates, v.description].filter(Boolean).join(' — '))),
        progress(v.progress), h('span', { class: 'rel-actions' }, ...act));
    }

    function release(v, all) {
      const others = all.filter(x => x.id !== v.id && !x.released && !x.archived);
      const body = h('div', {}), foot = h('div', { class: 'actions' });
      let close;
      const finish = x => { close(); load(); toast(x.moved ? 'Released. ' + x.moved + ' issue(s) moved on.' : 'Released'); };
      async function go(extra) {
        const x = await api('POST', '/api/v1/versions/' + v.id + '/release', extra);
        if (x.status !== 200) { toast(errText(x)); return; }
        if (x.data.released) { finish(x.data); return; }
        warn(x.data.unresolved);
      }
      function warn(open) {
        clear(body); clear(foot);
        const target = h('select', { 'aria-label': 'Move unfinished issues to' }, others.map(o => h('option', { value: o.id }, o.name)));
        body.append(h('p', { role: 'alert' }, open.length + ' issue(s) in ' + v.name + ' are not done:'),
          h('ul', {}, ...open.map(i => h('li', {}, h('a', { href: '#/issue/' + i.key }, i.key), ' ' + i.summary + ' (' + i.status + ')'))));
        if (others.length) body.append(h('label', {}, 'Move them to ', target));
        foot.append(h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
          others.length ? h('button', { class: 'btn primary', onclick: () => go({ move_to: target.value }) }, 'Move and release') : null,
          h('button', { class: 'btn', onclick: () => go({ confirm: true }) }, 'Release anyway'));
      }
      body.append(h('p', {}, 'Release ' + v.name + '?'));
      foot.append(h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: () => go({}) }, 'Release'));
      close = modal(h('div', {}, h('h2', {}, 'Release ' + v.name), body, foot));
    }

    function del(v, all) {
      const others = all.filter(x => x.id !== v.id);
      const target = h('select', { 'aria-label': 'Move its issues to' }, h('option', { value: '' }, 'Nowhere: just remove it'), others.map(o => h('option', { value: o.id }, o.name)));
      let close;
      close = modal(h('div', {}, h('h2', {}, 'Delete ' + v.name), h('p', {}, 'Issues that name this version lose it.'), h('label', {}, 'Move its issues to ', target),
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
          const x = await api('DELETE', '/api/v1/versions/' + v.id + (target.value ? '?move_to=' + target.value : ''));
          if (x.status === 200) { close(); load(); } else toast(errText(x));
        } }, 'Delete'))));
    }

    async function notes(v) {
      const r = await api('GET', '/api/v1/versions/' + v.id + '/notes');
      if (r.status !== 200) { toast(errText(r)); return; }
      let close;
      close = modal(h('div', {}, h('h2', {}, 'Release notes: ' + v.name), h('pre', { class: 'rel-notes', tabindex: 0, 'aria-label': 'Release notes text' }, r.data.markdown),
        h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: async () => { try { await navigator.clipboard.writeText(r.data.markdown); toast('Copied'); } catch (e) { toast('Select the text and copy it'); } } }, 'Copy'), h('button', { class: 'btn primary', onclick: () => close() }, 'Close'))));
    }

    const name = h('input', { type: 'text', 'aria-label': 'Version name', placeholder: '2.1.0', maxlength: 60 });
    const start = h('input', { type: 'date', 'aria-label': 'Start date' });
    const due = h('input', { type: 'date', 'aria-label': 'Release date' });
    const desc = h('input', { type: 'text', 'aria-label': 'Description', placeholder: 'Description (optional)', style: 'min-width:220px' });
    form.append(name, start, due, desc, h('button', { class: 'btn primary', onclick: async () => {
      msg.textContent = '';
      const r = await api('POST', '/api/v1/projects/' + key + '/versions', { name: name.value, start_date: start.value, release_date: due.value, description: desc.value });
      if (r.status === 201) { name.value = desc.value = start.value = due.value = ''; load(); } else msg.textContent = errText(r);
    } }, 'Add version'));
    showArchived.addEventListener('change', load);
    app.append(topbar(), h('div', { class: 'page' }, h('div', { class: 'chips' }, h('h2', { style: 'margin:0' }, 'Releases'), sel, h('label', { for: 'rel-arch' }, showArchived, ' Show archived'), h('a', { href: '#/portal-admin/' + key }, 'Customer portal settings'), h('a', { href: '#/queues/' + key }, 'Queues')), list, h('h3', { style: 'margin-top:16px' }, 'New version'), form, msg));
    await load();
  }

  A.EXT.routes.push({ re: /^#\/releases\/([A-Za-z][A-Za-z0-9]+)\/?$/, run: m => releasesView(m[1]) });
  A.EXT.routes.push({ re: /^#\/releases\/?$/, run: () => { location.hash = '#/releases/' + (S.projects[0] ? S.projects[0].key : ''); } });
})();
