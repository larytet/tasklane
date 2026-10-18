import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

// An administrator session for setting data up through the API; the UI is driven as an ordinary member.
async function admin(browser) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const r = await ctx.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } });
  expect(r.status()).toBe(200);
  const call = async (method, path, data) => {
    const res = await ctx.request.fetch(path, { method, headers: H, data });
    let body = null; try { body = await res.json(); } catch (e) { /* no body */ }
    return { status: res.status(), body };
  };
  return { ctx, call };
}

async function scrumWithIssues(adm, n, titles = []) {
  const b = (await adm.call('POST', '/api/v1/projects/ABC/boards', { name: 'Scrum ' + uniq(), kind: 'scrum' })).body;
  const sp = (await adm.call('POST', `/api/v1/boards/${b.id}/sprints`, { name: 'Sprint ' + uniq(), goal: 'ship it' })).body;
  const keys = [];
  for (let i = 0; i < n; i++) {
    const r = await adm.call('POST', '/api/v1/issues', { project: 'ABC', type: 'Task', summary: titles[i] || `card ${uniq()}` });
    keys.push(r.body.key);
  }
  return { board: b, sprint: sp, keys };
}

test('BOARD: drag a card to another column (a transition), keyboard move, a blocked move explains why, WIP limit shows', async ({ browser }) => {
  const adm = await admin(browser);
  const { board, sprint, keys } = await scrumWithIssues(adm, 3, ['Alpha card', 'Bravo card', 'Charlie card']);
  await adm.call('POST', `/api/v1/boards/${board.id}/plan`, { sprint: sprint.id, keys });
  await adm.call('POST', `/api/v1/sprints/${sprint.id}/start`);
  const get = await adm.call('GET', `/api/v1/boards/${board.id}`);
  const col = n => get.body.columns.find(c => c.name === n);
  await adm.call('PUT', `/api/v1/boards/${board.id}/columns`, { columns: [
    { name: 'To Do', statuses: ['To Do'] }, { name: 'In Progress', statuses: ['In Progress'], max_wip: 1 }, { name: 'Done', statuses: ['Done'] }] });
  expect(col('To Do').count).toBe(3);
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  await p.goto(`/#/board/${board.id}`);
  const column = name => p.getByRole('region', { name, exact: true });
  await expect(column('To Do').getByRole('listitem')).toHaveCount(3);
  // drag with the mouse
  await column('To Do').getByRole('listitem').filter({ hasText: 'Alpha card' }).dragTo(column('In Progress').getByRole('list'));
  await expect(column('In Progress').getByRole('listitem').filter({ hasText: 'Alpha card' })).toBeVisible();
  await expect(p.getByText(/moved by "Start progress"/)).toBeVisible();
  // the same move with the keyboard-reachable control
  await p.getByLabel(`Move ${keys[1]} to column`).selectOption({ label: 'In Progress' });
  await expect(column('In Progress').getByRole('listitem')).toHaveCount(2);
  await expect(column('In Progress')).toContainText('2 / 1 over the limit'); // the breach is text, not just colour
  // finish one, then try to move it back: the workflow has no such transition and the card explains why
  await p.getByLabel(`Move ${keys[0]} to column`).selectOption({ label: 'Done' });
  await expect(column('Done').getByRole('listitem')).toHaveCount(1);
  await p.getByLabel(`Move ${keys[0]} to column`).selectOption({ label: 'In Progress' });
  await expect(p.getByText(/no transition from "Done"/)).toBeVisible();
  await expect(column('Done').getByRole('listitem')).toHaveCount(1); // the card did not move
  // the order inside a column persists across reloads
  await p.reload();
  await expect(column('Done').getByRole('listitem')).toHaveCount(1);
  await expect(column('To Do').getByRole('listitem')).toHaveCount(1);
});

test('BOARD: quick filters toggle and a board with no active sprint says how to start one', async ({ browser }) => {
  const adm = await admin(browser);
  const { board, keys } = await scrumWithIssues(adm, 1);
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  await p.goto(`/#/board/${board.id}`);
  await expect(p.getByText('No active sprint has issues')).toBeVisible();
  await expect(p.getByRole('link', { name: 'Backlog' }).first()).toBeVisible();
  // a kanban board with a quick filter
  const kb = (await adm.call('POST', '/api/v1/projects/ABC/boards', { name: 'Flow ' + uniq(), kind: 'kanban' })).body;
  await adm.call('PATCH', `/api/v1/boards/${kb.id}`, { quick_filters: [{ name: 'Only my issues', query: 'assignee = currentUser()' }] });
  await p.goto(`/#/board/${kb.id}`);
  const chip = p.getByRole('button', { name: 'Only my issues' });
  await expect(chip).toHaveAttribute('aria-pressed', 'false');
  await expect(p.getByRole('listitem').filter({ hasText: keys[0] })).toHaveCount(1); // a kanban board shows the whole project until a filter is on
  await chip.click();
  await expect(p.getByRole('button', { name: 'Only my issues' })).toHaveAttribute('aria-pressed', 'true');
  await expect(p.getByRole('listitem')).toHaveCount(0); // nothing is assigned to Jana, so the chip empties the board instead of showing everything
});

test('BACKLOG: plan with the move control, start and complete a sprint with a preview, carry-over shows', async ({ browser }) => {
  const adm = await admin(browser);
  const { board, sprint, keys } = await scrumWithIssues(adm, 2, ['Plan me', 'Stay in the backlog']);
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  await p.goto(`/#/backlog/${board.id}`);
  const panel = name => p.getByRole('region', { name, exact: true });
  await expect(panel('Backlog').getByRole('listitem').filter({ hasText: 'Plan me' })).toBeVisible();
  await p.getByLabel(`Move ${keys[0]} to`).selectOption({ label: sprint.name });
  await expect(panel(sprint.name).getByRole('listitem').filter({ hasText: 'Plan me' })).toBeVisible();
  await expect(panel('Backlog').getByRole('listitem').filter({ hasText: 'Plan me' })).toHaveCount(0);
  // a member cannot start the sprint: the button is not offered
  await expect(p.getByRole('button', { name: 'Start sprint' })).toHaveCount(0);
  // inline create at the bottom of a panel
  await p.getByLabel(`Create an issue in ${sprint.name}`).fill('Created inline');
  await p.getByLabel(`Create an issue in ${sprint.name}`).press('Enter');
  await expect(panel(sprint.name).getByRole('listitem').filter({ hasText: 'Created inline' })).toBeVisible();
  // the administrator starts and completes it
  const boss = await browser.newContext();
  boss.on('page', watch);
  const bp = await boss.newPage();
  await bp.goto('/');
  await bp.getByLabel('Email').fill('admin@example.com');
  await bp.getByLabel('Password').fill('adminpass1');
  await bp.getByRole('button', { name: 'Sign in' }).click();
  await expect(bp.getByRole('button', { name: '+ Create' })).toBeVisible();
  await bp.goto(`/#/backlog/${board.id}`);
  await bp.getByRole('button', { name: 'Start sprint' }).first().click();
  await expect(bp.getByRole('button', { name: 'Complete sprint' })).toBeVisible();
  await bp.getByRole('button', { name: 'Complete sprint' }).click();
  const dlg = bp.getByRole('dialog');
  await bp.getByLabel('Move unfinished issues to').selectOption({ label: 'The backlog' });
  await expect(dlg).toContainText('Unfinished: 2 → Backlog'); // the preview is the result
  await dlg.getByRole('button', { name: 'Complete sprint' }).click();
  await expect(bp.getByText(`${sprint.name} completed.`)).toBeVisible();
  await expect(bp.getByRole('region', { name: 'Backlog', exact: true }).getByRole('listitem').filter({ hasText: 'Plan me' })).toContainText('carried over 1×');
});

test('REPORTS: charts come with a data table, velocity and flow reports render', async ({ browser }) => {
  const adm = await admin(browser);
  const { board, sprint, keys } = await scrumWithIssues(adm, 2);
  await adm.call('POST', `/api/v1/boards/${board.id}/plan`, { sprint: sprint.id, keys });
  await adm.call('POST', `/api/v1/sprints/${sprint.id}/start`);
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  await p.goto(`/#/reports/${board.id}/burndown`);
  const fig = p.getByRole('figure');
  await expect(fig.getByRole('img', { name: /^Burndown: / })).toBeVisible();
  await fig.getByText('Data table').click();
  await expect(fig.getByRole('table')).toContainText('Remaining');
  await p.getByLabel('Report').selectOption({ label: 'Sprint report' });
  await expect(p.getByRole('heading', { name: /Not completed \(2\)/ })).toBeVisible();
  await p.getByLabel('Report').selectOption({ label: 'Cumulative flow' });
  await expect(p.getByRole('img', { name: 'Cumulative flow' })).toBeVisible();
  await p.getByLabel('Report').selectOption({ label: 'Workload' });
  await expect(p.getByRole('table')).toContainText('Unassigned');
  await p.getByLabel('Report').selectOption({ label: 'Velocity' });
  await expect(p.getByText('No sprint has been completed yet.')).toBeVisible();
});

for (const theme of ['dark', 'light']) {
  test(`A11Y/UI-04: board, backlog and reports are accessible and fit a phone, ${theme}`, async ({ browser }) => {
    const adm = await admin(browser);
    const { board, sprint, keys } = await scrumWithIssues(adm, 2);
    await adm.call('POST', `/api/v1/boards/${board.id}/plan`, { sprint: sprint.id, keys });
    await adm.call('POST', `/api/v1/sprints/${sprint.id}/start`);
    const jana = await newUser(browser, 'jana@example.com', { viewport: { width: 390, height: 800 } });
    const p = jana.page;
    await p.addInitScript(t => { try { localStorage.setItem('tl-theme', t); } catch (e) { /* ignore */ } }, theme);
    for (const [url, ready] of [[`/#/board/${board.id}`, p.getByRole('region', { name: 'To Do', exact: true })], [`/#/backlog/${board.id}`, p.getByRole('region', { name: sprint.name, exact: true })], [`/#/reports/${board.id}/burndown`, p.getByRole('figure')]]) {
      await p.goto('/?t=' + theme + url); // the query changes the document so the init script runs
      await expect(ready).toBeVisible();
      const r = await new AxeBuilder({ page: p }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
      const bad = r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
      expect(bad.map(v => `${v.id}: ${v.help} ${v.nodes.slice(0, 2).map(n => n.target.join(' ')).join(' | ')}`), url).toEqual([]);
      const overflow = await p.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      expect(overflow, 'the page itself must not scroll sideways on a phone; the board scrolls inside').toBeLessThanOrEqual(1);
    }
  });
}

// BLOB-03 in the browser: with an S3 backend a large file goes straight to the store in parts. The e2e server uses the
// filesystem backend, so the store and the two control calls are played by route handlers; what is checked is the
// client's protocol: parts of the announced size, their ETags returned in order, completion last, and a fall back to the
// ordinary upload when the server says it has no direct uploads.
test('BLOB-03 client: large files use presigned parts and fall back to the server upload when direct is unavailable', async ({ browser }) => {
  const adm = await admin(browser);
  const key = (await adm.call('POST', '/api/v1/issues', { project: 'ABC', type: 'Task', summary: 'direct ' + uniq() })).body.key;
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  const log = [];
  // an S3 installation sends the page a CSP that allows its bucket origin (unit-tested); give the page the same here
  await p.route(u => u.pathname === '/', async route => {
    const resp = await route.fetch();
    const headers = resp.headers();
    headers['content-security-policy'] = headers['content-security-policy'].replace("connect-src 'self'", "connect-src 'self' https://store.test");
    await route.fulfill({ response: resp, headers });
  });
  await p.route('**/api/v1/issues/*/uploads/direct', async route => {
    log.push('start');
    await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify({ id: 'u1', mode: 'multipart', part_size: 8 << 20, parts: [1, 2, 3].map(n => ({ n, url: `https://store.test/part/${n}` })), expires_at: new Date(Date.now() + 3600e3).toISOString() }) });
  });
  await p.route('https://store.test/part/*', async route => {
    const n = route.request().url().split('/').pop();
    log.push('part' + n + ':' + route.request().postDataBuffer().length);
    await route.fulfill({ status: 200, headers: { 'Access-Control-Allow-Origin': '*', 'Access-Control-Expose-Headers': 'ETag', ETag: `"etag${n}"` }, body: '' });
  });
  await p.route('**/api/v1/uploads/direct/u1/complete', async route => {
    const body = route.request().postDataJSON();
    log.push('complete:' + body.parts.sort((a, b) => a.n - b.n).map(x => x.n + x.etag).join(','));
    await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify({ id: 'a1', name: 'big.bin' }) });
  });
  await p.goto(`/#/issue/${key}`);
  await p.reload(); // a hash change keeps the old document, and with it the old CSP: load the page again under the new one
  await expect(p.getByRole('heading', { level: 1 })).toBeVisible();
  const size = 17 << 20; // 8 + 8 + 1 MiB
  await p.locator('#file').setInputFiles({ name: 'big.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(size, 7) });
  await expect.poll(() => log.some(l => l.startsWith('complete:'))).toBe(true);
  expect(log[0]).toBe('start');
  expect(log.filter(l => l.startsWith('part')).map(l => l.replace(/^part\d:/, '')).sort()).toEqual([String(1 << 20), String(8 << 20), String(8 << 20)]);
  expect(log[log.length - 1]).toBe('complete:1"etag1",2"etag2",3"etag3"'); // completion is last, with every part's ETag
  // the server has no direct uploads: the client falls back to the resumable upload without bothering the person
  await p.unroute('**/api/v1/issues/*/uploads/direct');
  await p.reload();
  await expect(p.getByRole('heading', { level: 1 })).toBeVisible();
  await p.locator('#file').setInputFiles({ name: 'ten.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(10 << 20, 3) });
  await expect(p.getByText('ten.bin')).toBeVisible();
});

// GIT-UI: a signed webhook reaches the ticket's Development section, collapsed with a one-line summary; the page never
// talks to the provider.
test('GIT: a signed webhook shows on the ticket as a one-line summary that expands', async ({ browser }) => {
  const { createHmac } = await import('node:crypto');
  const adm = await admin(browser);
  const conn = (await adm.call('POST', '/api/v1/git/connections', { name: 'gh-' + uniq(), provider: 'github' })).body;
  await adm.call('POST', `/api/v1/git/connections/${conn.connection.id}/repos`, { full_name: 'acme/shop', projects: ['ABC'] });
  const key = (await adm.call('POST', '/api/v1/issues', { project: 'ABC', type: 'Task', summary: 'git ui ' + uniq() })).body.key;
  const send = async (event, id, payload) => {
    const body = JSON.stringify(payload);
    const sig = 'sha256=' + createHmac('sha256', conn.webhook_secret).update(body).digest('hex');
    const r = await adm.ctx.request.post('/api/v1/git/webhooks/' + conn.connection.id, { headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': sig, 'X-GitHub-Event': event, 'X-GitHub-Delivery': id }, data: body });
    expect(r.status()).toBe(200);
  };
  const now = new Date().toISOString();
  await send('pull_request', 'd-' + uniq(), { action: 'opened', repository: { full_name: 'acme/shop' }, pull_request: { number: 5, title: `${key} add the thing`, body: '', state: 'open', draft: false, merged: false, html_url: 'https://github.com/acme/shop/pull/5', user: { login: 'octo' }, head: { ref: 'thing', sha: 'abc1234' }, base: { ref: 'main' }, updated_at: now } });
  await send('check_run', 'c-' + uniq(), { repository: { full_name: 'acme/shop' }, check_run: { name: 'ci', head_sha: 'abc1234', status: 'completed', conclusion: 'success', html_url: 'https://github.com/acme/shop/runs/1', completed_at: now } });
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  await p.goto(`/#/issue/${key}`);
  const dev = p.locator('details.dev');
  await expect(dev.locator('summary')).toContainText('1 PR open, build passing'); // the processor runs once a second
  await expect(dev.getByText(`${key} add the thing`)).toBeHidden(); // collapsed by default
  await dev.locator('summary').click();
  await expect(dev.getByRole('link', { name: `${key} add the thing` })).toBeVisible();
  await expect(dev).toContainText('ci');
  // a person with no link to code sees no section at all
  const other = (await adm.call('POST', '/api/v1/issues', { project: 'ABC', type: 'Task', summary: 'no code ' + uniq() })).body.key;
  await p.goto(`/#/issue/${other}`);
  await expect(p.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(p.locator('details.dev')).toBeHidden();
});

// MIR-07/08 in the browser: a mirrored issue shows a banner with the freshness and an "Edit in Jira" link, and offers no
// way to edit. A tiny Jira stands in for the site (the Go suites use a fuller double).
test('JIRA: a mirrored issue is read-only in the UI with an Edit in Jira link', async ({ browser }) => {
  const http = await import('node:http');
  const now = new Date().toISOString().replace('Z', '+0000');
  const issue = { id: '70001', key: 'MIR-7', fields: { summary: 'Mirrored from the site', created: now, updated: now, project: { key: 'MIR' }, issuetype: { name: 'Task' }, status: { name: 'To Do', statusCategory: { key: 'new' } },
    priority: { name: 'High' }, labels: ['jira'], reporter: { accountId: 'acc-r', displayName: 'Rita R.' }, assignee: null, description: { type: 'doc', version: 1, content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Body written in Jira.' }] }] },
    parent: null, issuelinks: [], security: null, comment: { comments: [], total: 0 }, resolutiondate: null } };
  const site = http.createServer((req, res) => {
    const u = new URL(req.url, 'http://x');
    const auth = req.headers.authorization || '';
    res.setHeader('Content-Type', 'application/json');
    if (auth !== 'Basic ' + Buffer.from('svc@example.com:tok').toString('base64')) { res.statusCode = 401; return res.end('{}'); }
    if (u.pathname === '/rest/api/3/myself') return res.end(JSON.stringify({ accountId: 'acc-svc', displayName: 'Svc', emailAddress: 'svc@example.com', timeZone: 'UTC' }));
    if (u.pathname === '/rest/api/3/search/jql') return res.end(JSON.stringify({ issues: [{ id: issue.id, key: issue.key, fields: issue.fields, changelog: { histories: [], total: 0 } }] }));
    if (u.pathname === '/rest/api/3/project/MIR/role') return res.end('{}');
    res.statusCode = 404; res.end('{}');
  });
  await new Promise(r => site.listen(0, '127.0.0.1', r));
  const url = 'http://127.0.0.1:' + site.address().port;
  try {
    const adm = await admin(browser);
    const conn = (await adm.call('POST', '/api/v1/jira/connections', { name: 'site-' + uniq(), site_url: url, email: 'svc@example.com', token: 'tok', allow_private: true })).body;
    expect(conn.connection).toBeTruthy();
    const key = 'M' + uniq().toUpperCase().slice(0, 3);
    const pr = await adm.call('POST', `/api/v1/jira/connections/${conn.connection.id}/projects`, { jira_key: 'MIR', key, name: 'Mirror' });
    expect(pr.status).toBe(201);
    const sync = await adm.call('POST', `/api/v1/jira/projects/${key}/sync`);
    expect(sync.status).toBe(200);
    expect(sync.body.issues).toBe(1);
    const boss = await browser.newContext();
    boss.on('page', watch);
    const p = await boss.newPage();
    await p.goto('/');
    await p.getByLabel('Email').fill('admin@example.com');
    await p.getByLabel('Password').fill('adminpass1');
    await p.getByRole('button', { name: 'Sign in' }).click();
    await expect(p.getByRole('button', { name: '+ Create' })).toBeVisible();
    await p.goto(`/#/issue/${key}-7`);
    await expect(p.getByLabel('Summary')).toHaveValue('Mirrored from the site');
    const banner = p.getByRole('status').filter({ hasText: 'Mirrored from Jira' });
    await expect(banner).toContainText('read-only');
    await expect(banner.getByRole('link', { name: 'Edit in Jira' })).toHaveAttribute('href', url + '/browse/MIR-7');
    await expect(p.getByText('Body written in Jira.')).toBeVisible();
    // nothing can be edited: no Actions menu, no Edit button, a read-only summary and no reply box
    await expect(p.getByRole('button', { name: /Actions/ })).toHaveCount(0);
    await expect(p.getByRole('button', { name: 'Edit', exact: true })).toHaveCount(0);
    await expect(p.getByLabel('Summary')).toHaveAttribute('readonly', '');
    await expect(p.getByLabel('Reply')).toHaveCount(0);
    await expect(p.getByLabel('Priority')).toBeDisabled();
  } finally {
    site.close();
  }
});
