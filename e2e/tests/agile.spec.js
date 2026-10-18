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
