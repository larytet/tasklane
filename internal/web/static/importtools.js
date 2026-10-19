// Import from another tool: GitHub Issues, Trello, Linear, Asana, or any JSON through a path mapping. Pick a project, the tool and its export file; a dry run says what would happen.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, clear, api, toast, topbar, S } = A;
  const errText = r => (r.data && r.data.error) || 'Something went wrong';
  const HELP = {
    github: 'The answer of GET /repos/OWNER/REPO/issues?state=all (a JSON file), or an object with "issues" and "comments". Milestones become versions; pull requests are skipped.',
    trello: 'A board\'s JSON export (Menu, More, Print and export, Export as JSON). Lists become statuses, checklist items subtasks.',
    linear: 'Linear\'s CSV export (Settings, Import / Export). Projects become epics, sub-issues subtasks, cycles labels.',
    asana: 'Asana\'s CSV export of a project. Sections become statuses, tasks with a parent task subtasks.',
    json: 'Any JSON file. Say in the mapping where the issues are (items) and the path of each field inside one issue; status_map, type_map and priority_map are optional.',
  };
  const parseMap = t => { const m = {}; for (const line of t.split('\n')) { const i = line.indexOf('='); if (i > 0) m[line.slice(0, i).trim()] = line.slice(i + 1).trim(); } return m; };

  async function view(key) {
    const app = clear(document.getElementById('app'));
    const tools = (await api('GET', '/api/v1/import/tools')).data || {};
    const proj = h('select', { 'aria-label': 'Project' }, S.projects.map(p => h('option', { value: p.key, selected: p.key === key.toUpperCase() }, p.key + ' · ' + p.name)));
    const tool = h('select', { 'aria-label': 'Import from' }, Object.entries(tools).map(([k, n]) => h('option', { value: k }, n)));
    const help = h('p', { class: 'muted' }, HELP[tool.value] || '');
    const mapping = h('textarea', { id: 'imp-mapping', rows: 10, spellcheck: false, placeholder: '{\n  "items": "$.data.tickets",\n  "fields": {"key": "ref", "summary": "fields.title", "status": "fields.state"},\n  "comments": "notes",\n  "comment_fields": {"body": "text", "author": "by"},\n  "status_map": {"Working": "In Progress"}\n}' });
    const mapBox = h('div', { hidden: true }, h('label', { for: 'imp-mapping' }, 'The mapping (JSON; paths such as $.data.items, fields.title, tags[0], $..id)'), mapping);
    const sync = () => { help.textContent = HELP[tool.value] || ''; mapBox.hidden = tool.value !== 'json'; };
    tool.addEventListener('change', sync);
    const file = h('input', { type: 'file', id: 'imp-file' });
    const users = h('textarea', { id: 'imp-users', rows: 4, placeholder: 'octocat = octo@example.com\nAna Novak = ana@example.com' });
    const dry = h('input', { type: 'checkbox', id: 'imp-dry', checked: true });
    const out = h('div', { 'aria-live': 'polite' });
    const run = async dryRun => {
      clear(out);
      if (!file.files[0]) { out.append(h('p', { class: 'err', role: 'alert' }, 'Choose the export file first.')); return; }
      const data = await file.files[0].text();
      out.append(h('p', {}, 'Working…'));
      const body = { project: proj.value, data, dry_run: dryRun, user_map: parseMap(users.value) };
      if (tool.value === 'json') {
        let m;
        try { m = JSON.parse(mapping.value); } catch (e) { clear(out); out.append(h('p', { class: 'err', role: 'alert' }, 'The mapping is not valid JSON: ' + e.message)); return; }
        for (const k of ['status_map', 'type_map', 'priority_map']) if (m[k]) { body[k] = m[k]; delete m[k]; }
        body.mapping = m;
      }
      const r = await api('POST', '/api/v1/import/' + tool.value, body);
      clear(out);
      if (r.status !== 200) { out.append(h('p', { class: 'err', role: 'alert' }, errText(r))); return; }
      const rep = r.data;
      const list = (title, items, cls) => items && items.length ? [h('h4', {}, title + ' (' + items.length + ')'), h('ul', { class: cls || '' }, items.slice(0, 50).map(x => h('li', {}, x))), items.length > 50 ? h('p', { class: 'muted' }, 'and ' + (items.length - 50) + ' more') : null] : [];
      out.append(h('h3', {}, rep.dry_run ? 'Dry run: nothing was changed' : 'Imported'),
        h('p', {}, Object.entries(rep.counts || {}).map(([k, v]) => k.replace(/_/g, ' ') + ': ' + v).join(' · ') || 'Nothing to import.'),
        ...list('Not mapped', rep.gaps), ...list('Warnings', rep.warnings), ...list('Problems', rep.errors, 'err'),
        rep.dry_run ? h('p', {}, h('button', { class: 'btn primary', onclick: () => run(false) }, 'Import for real')) :
          h('p', {}, 'Import ' + rep.job_id + '. ', h('button', { class: 'btn ghost', onclick: async () => { const x = await api('POST', '/api/v1/import/' + rep.job_id + '/rollback'); if (x.status === 200) { toast('Rolled back: ' + x.data.issues_removed + ' issue(s) removed'); clear(out); } else toast(errText(x)); } }, 'Roll this import back')));
    };
    sync();
    app.append(topbar(), h('div', { class: 'page' }, h('h2', {}, 'Import from another tool'),
      h('div', { class: 'chips' }, h('label', {}, 'Into ', proj), h('label', {}, 'From ', tool)), help,
      h('label', { for: 'imp-file' }, 'The export file'), file, mapBox,
      h('label', { for: 'imp-users' }, 'Who is who (one per line: the name or login in the export = an email address). Anyone left out is reported and left unassigned.'), users,
      h('div', { class: 'actions' }, h('label', { for: 'imp-dry' }, dry, ' Dry run first'), h('button', { class: 'btn primary', onclick: () => run(dry.checked) }, 'Check and import')), out));
  }
  A.EXT.routes.push({ re: /^#\/import\/([A-Za-z][A-Za-z0-9]+)\/?$/, run: m => view(m[1]) });
  A.EXT.routes.push({ re: /^#\/import\/?$/, run: () => view(S.projects[0] ? S.projects[0].key : '') });
})();
