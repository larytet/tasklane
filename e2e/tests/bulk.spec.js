import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const view = (page, key) => page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json()), key);
async function listOf(page, n, word = 'bulk') {
  const keys = [];
  for (let i = 0; i < n; i++) keys.push(await createIssue(page, `${word} ${i} ${Date.now()}`, 'x'));
  await page.goto('/#/?q=' + encodeURIComponent('key in (' + keys.join(', ') + ')') + '&syntax=tql');
  await expect(page.getByRole('listitem')).toHaveCount(n);
  const order = await page.locator('[role=list] > .row[data-key]').evaluateAll(rs => rs.map(r => r.dataset.key));
  return order;
}
const bar = page => page.getByRole('toolbar', { name: 'Selected issues' });

test('BULK-01, BULK-02: checkbox, x, shift-click range, the page checkbox and Clear; the count always equals the checked rows; no "select all matching" when the page holds every match', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const order = await listOf(page, 6);
  await expect(bar(page)).toBeHidden();
  await page.getByLabel('Select ' + order[0]).check();
  await expect(bar(page)).toContainText('1 selected');
  await page.getByLabel('Select ' + order[3]).click({ modifiers: ['Shift'] }); // 0..3
  await expect(bar(page)).toContainText('4 selected');
  for (const k of order.slice(0, 4)) await expect(page.getByLabel('Select ' + k)).toBeChecked();
  await expect(page.getByLabel('Select all on this page')).toHaveJSProperty('indeterminate', true);
  // the shift-clicked row is the current one; k walks back to the first, and x takes it off
  for (let i = 0; i < 4; i++) await page.keyboard.press('k');
  await page.keyboard.press('x');
  await expect(bar(page)).toContainText('3 selected');
  await page.getByLabel('Select all on this page').check();
  await expect(bar(page)).toContainText('6 selected');
  const checked = await page.locator('[role=list] input.rowsel:checked').count();
  expect(checked).toBe(6);
  await expect(page.getByText(/select all \d+ matching/i)).toHaveCount(0); // everything that matches is on the page, so there is nothing more to select (UX-BULK-ALL offers it past one page)
  await bar(page).getByRole('button', { name: 'Clear' }).click();
  await expect(bar(page)).toBeHidden();
  await expect(page.locator('[role=list] input.rowsel:checked')).toHaveCount(0);
  await ctx.close();
});

for (const theme of ['dark', 'light']) {
  test(`BULK-08: the bar stays in view, works by keyboard, offers only valid transitions, applies a clean plan at once with Undo (${theme})`, async ({ browser }) => {
    const { page, ctx } = await newUser(browser, 'jana@example.com', { viewport: { width: 1280, height: 600 } });
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    const order = await listOf(page, 14, 'bar ' + theme);
    await page.getByLabel('Select all on this page').check();
    await expect(bar(page)).toContainText('14 selected');
    await new AxeBuilder({ page }).include('.bulkbar').analyze().then(r => expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]));
    await page.mouse.wheel(0, 2000);
    await expect(bar(page)).toBeInViewport();
    // Transition: only what the selection can take now (To Do: Start progress and Resolve)
    await bar(page).getByRole('button', { name: 'Transition' }).focus();
    await page.keyboard.press('Enter');
    const pick = page.getByRole('dialog', { name: 'Transition 14 issues' });
    await expect(pick.getByRole('option')).toHaveText([/^Start progress → In Progress/, /^Resolve → Done/]);
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter'); // Resolve
    await expect(page.locator('#toasts')).toContainText('14 issues moved to Done.');
    for (const k of [order[0], order[13]]) expect((await view(page, k)).issue.status).toBe('Done');
    // one Undo for the batch
    await page.locator('#toasts .toast').filter({ hasText: '14 issues moved to Done.' }).getByRole('button', { name: 'Undo' }).click();
    await expect(page.locator('#toasts')).toContainText('Undone.');
    for (const k of [order[0], order[13]]) expect((await view(page, k)).issue.status).toBe('To Do');
    await ctx.close();
  });
}

test('BULK-05, BULK-08: some issues cannot take it: the plan is shown first, the rest are applied, the failures are listed per issue', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const order = await listOf(page, 3);
  // one of them is done already, so Resolve is not available for it
  const v = await view(page, order[1]);
  await page.evaluate(async ({ k, id }) => fetch('/api/v1/issues/' + k + '/transitions', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ transition: id }) }), { k: order[1], id: v.transitions.find(t => t.name === 'Resolve').id });
  await page.reload();
  await expect(page.getByRole('listitem')).toHaveCount(3);
  await page.getByLabel('Select all on this page').check();
  await bar(page).getByRole('button', { name: 'Transition' }).click();
  await page.getByRole('dialog', { name: /^Transition 3 issues/ }).getByRole('option', { name: /^Resolve/ }).click();
  const plan = page.getByRole('dialog', { name: 'Check before applying' });
  await expect(plan).toContainText('2 of 3 issues can be changed. 1 cannot.');
  await expect(plan.locator('li').filter({ hasText: order[1] })).toContainText('is not available from status "Done"');
  await expect(plan.getByRole('button', { name: 'Apply to 2' })).toBeVisible();
  const r = await new AxeBuilder({ page }).include('.dialog').analyze();
  expect(r.violations.filter(x => ['serious', 'critical'].includes(x.impact))).toEqual([]);
  await plan.getByRole('button', { name: 'Apply to 2' }).click();
  await expect(page.locator('#toasts')).toContainText('2 issues moved to Done.');
  const rep = page.getByRole('dialog', { name: '1 issue was not changed' });
  await expect(rep).toContainText(order[1]);
  await rep.getByRole('button', { name: 'Close' }).click();
  await ctx.close();
});

test('BULK-07, HOVER-02: a viewer sees the bar with its actions off and the reason, and no hover actions', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const keys = [await createIssue(jana.page, 'viewer bulk a', 'x'), await createIssue(jana.page, 'viewer bulk b', 'x')];
  const { page, ctx } = await newUser(browser, 'viewer@example.com');
  await page.goto('/#/?q=' + encodeURIComponent('key in (' + keys.join(', ') + ')') + '&syntax=tql');
  await expect(page.getByRole('listitem')).toHaveCount(2);
  await page.getByLabel('Select all on this page').check();
  await expect(bar(page)).toContainText('You can only view these issues');
  for (const name of ['Assign', 'Transition', 'Label', 'Move', 'Archive', 'Delete']) await expect(bar(page).getByRole('button', { name })).toBeDisabled();
  const requests = [];
  page.on('request', r => { if (r.method() !== 'GET') requests.push(r.url()); });
  await page.getByRole('listitem').first().hover();
  await page.waitForTimeout(500);
  await expect(page.locator('.hover-acts button')).toHaveCount(0);
  expect(requests).toEqual([]);
  await jana.ctx.close(); await ctx.close();
});

test('HOVER-01, HOVER-04: assign, mark done and watch from a row, by mouse and by keyboard focus, without opening the issue', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com', { viewport: { width: 1280, height: 800 } });
  const order = await listOf(page, 2, 'hover');
  const row = page.getByRole('listitem').filter({ hasText: order[0] });
  await row.hover();
  const done = row.getByRole('button', { name: 'Mark ' + order[0] + ' done' });
  await expect(done).toBeVisible();
  for (const b of await row.locator('.hover-acts button').all()) {
    const box = await b.boundingBox();
    expect(box.height, 'hit target').toBeGreaterThanOrEqual(32);
    expect(box.width, 'hit target').toBeGreaterThanOrEqual(32);
  }
  await done.click();
  await expect(page.locator('#toasts')).toContainText(order[0] + ' moved to Done.');
  await expect(page).not.toHaveURL(/#\/issue\//);
  expect((await view(page, order[0])).issue.status).toBe('Done');
  // watch, then stop
  const row2 = page.getByRole('listitem').filter({ hasText: order[1] });
  await row2.hover();
  // the reporter watches their own issue already: the button says so, and one click flips it as an explicit choice
  const wbtn = row2.getByRole('button', { name: new RegExp('(Watch|Stop watching) ' + order[1] + '$') });
  const was = await wbtn.getAttribute('aria-pressed');
  await wbtn.click();
  await expect(wbtn).toHaveAttribute('aria-pressed', was === 'true' ? 'false' : 'true');
  const ws = await page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/watch', { credentials: 'same-origin' })).json()), order[1]);
  expect(ws.watching).toBe(was !== 'true');
  expect(ws.explicit || ws.muted).toBe(true);
  await expect(page).not.toHaveURL(/#\/issue\//);
  // by keyboard: focus on the row shows them, Tab reaches them, Enter acts
  await page.mouse.move(1270, 790);
  await row2.focus();
  await expect(row2.getByRole('button', { name: 'Assign ' + order[1] })).toBeVisible();
  await row2.getByRole('button', { name: 'Assign ' + order[1] }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('dialog', { name: 'Assign ' + order[1] })).toBeVisible();
  await page.keyboard.press('Enter'); // Me
  await expect(page.locator('#toasts')).toContainText(order[1] + ' assigned to you.');
  await expect(page).not.toHaveURL(/#\/issue\//);
  await ctx.close();
});

test('HOVER-03: on a phone there are no hover actions and a tap opens the row', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com', { viewport: { width: 390, height: 800 }, hasTouch: true, isMobile: true });
  const order = await listOf(page, 1, 'touch');
  const row = page.getByRole('listitem').first();
  await row.locator('.sum').tap();
  await expect(page).toHaveURL(new RegExp('#/issue/' + order[0] + '$'));
  await page.goBack();
  await expect(page.getByRole('listitem')).toHaveCount(1);
  await expect(page.locator('.hover-acts button')).toHaveCount(0);
  await ctx.close();
});

const total = (page, q) => page.evaluate(async q => (await (await fetch('/api/v1/issues?limit=1&syntax=tql&q=' + encodeURIComponent(q), { credentials: 'same-origin' })).json()).total, q);
for (const theme of ['dark', 'light']) {
  test(`UX-BULK-ALL: "Select all N matching" past one page runs the action as a job with progress, in both themes (${theme})`, async ({ browser }) => {
    const { page, ctx } = await newUser(browser, 'jana@example.com', { viewport: { width: 1280, height: 700 } });
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    const tag = 'sa' + theme[0] + Date.now(), added = 'added' + theme[0] + Date.now();
    await page.goto('/');
    await page.evaluate(async tag => {
      for (let i = 0; i < 104; i++) await fetch('/api/v1/issues', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ project: 'ABC', type: 'Task', summary: 'all ' + i, labels: [tag] }) });
    }, tag);
    await page.goto('/#/?q=' + encodeURIComponent('labels = ' + tag) + '&syntax=tql');
    await expect(page.getByRole('listitem')).toHaveCount(100);
    // nothing selected: no offer; a few rows: no offer; the whole page: the offer with the count
    await expect(page.getByRole('button', { name: /Select all \d+ matching/ })).toHaveCount(0);
    await page.getByLabel('Select all on this page').check();
    await expect(bar(page)).toContainText('100 selected');
    const all = bar(page).getByRole('button', { name: 'Select all 104 matching' });
    await expect(all).toBeVisible();
    // taking one row off withdraws the offer, and putting it back brings it again
    const first = (await page.locator('[role=list] > .row[data-key]').first().getAttribute('data-key'));
    await page.getByLabel('Select ' + first).uncheck();
    await expect(all).toBeHidden();
    await page.getByLabel('Select ' + first).check();
    await all.click();
    await expect(bar(page)).toContainText('All 104 matching selected');
    await new AxeBuilder({ page }).include('.bulkbar').analyze().then(r => expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]));
    await bar(page).getByRole('button', { name: 'Label' }).click();
    await page.getByLabel('Label', { exact: true }).fill(added);
    await page.getByRole('button', { name: 'Add' }).click();
    // confirmed first: it names the count and says there is no Undo
    const confirm = page.getByRole('dialog').filter({ hasText: 'Apply to all 104 matching issues?' });
    await expect(confirm).toContainText('no Undo');
    await new AxeBuilder({ page }).include('[role=dialog]').analyze().then(r => expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]));
    await confirm.getByRole('button', { name: 'Apply to all 104' }).click();
    const panel = page.getByRole('group', { name: 'Background job' });
    await expect(panel).toBeVisible();
    await expect(panel).toContainText(/Finished: 104 of 104 issues changed/, { timeout: 60000 });
    await new AxeBuilder({ page }).include('.jobpanel').analyze().then(r => expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]));
    expect(await total(page, 'labels = ' + added)).toBe(104); // all of them, not only the page that was shown
    await ctx.close();
  });
}

test('UX-BULK-ALL: issues that cannot take the change are skipped and listed with the reason', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com', { viewport: { width: 1280, height: 700 } });
  const tag = 'sf' + Date.now();
  await page.goto('/');
  await page.evaluate(async tag => {
    for (let i = 0; i < 102; i++) await fetch('/api/v1/issues', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ project: 'ABC', type: 'Task', summary: 'f ' + i, labels: [tag] }) });
  }, tag);
  await page.goto('/#/?q=' + encodeURIComponent('labels = ' + tag) + '&syntax=tql');
  await expect(page.getByRole('listitem')).toHaveCount(100);
  await page.getByLabel('Select all on this page').check();
  await bar(page).getByRole('button', { name: /Select all 102 matching/ }).click();
  await bar(page).getByRole('button', { name: 'Archive' }).click();
  await page.getByRole('button', { name: 'Apply to all 102' }).click();
  const panel = page.getByRole('group', { name: 'Background job' });
  await expect(panel).toContainText(/Finished: 0 of 102 issues changed, 102 skipped/, { timeout: 60000 }); // only finished work is archived
  await expect(panel.getByRole('listitem').first()).toContainText('only finished work is archived');
  await ctx.close();
});
