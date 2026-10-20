import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];
async function axe(page, what) {
  const r = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const bad = r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
  expect(bad.map(v => v.id + ': ' + v.nodes.slice(0, 2).map(n => n.target.join(' ')).join(', ')), what).toEqual([]);
}
const tag = () => 'kq' + [...Math.random().toString(36).slice(2, 7)].map(c => 'abcdefghij'[parseInt(c, 36) % 10]).join('');
const put = (page, data) => page.evaluate(async d => (await fetch('/api/v1/me/ui-prefs', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify(d) })).status, data);
const issue = (page, key) => page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json()).issue, key);
// a few fresh issues, listed by a search for their keys
async function listOf(page, n = 3) {
  const t = tag(), keys = [];
  for (let i = 0; i < n; i++) keys.push(await createIssue(page, `${t} item ${i}`, 'x'));
  await page.goto('/#/?q=' + encodeURIComponent('key in (' + keys.join(', ') + ')') + '&syntax=tql'); // by key: text search reads an index that may lag
  await expect(page.getByRole('listitem')).toHaveCount(n);
  const order = await page.locator('[role=list] > .row[data-key]').evaluateAll(rs => rs.map(r => r.dataset.key));
  return { t, keys, order };
}
const current = page => page.locator('.row[aria-current=true]').getAttribute('data-key');

test('KEY-01, KEY-11: j/k move the current row (kept in bounds, scrolled into view, said once), o and Enter open, / searches, c creates, g i and g b go', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(page);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('j');
  expect(await current(page)).toBe(order[0]);
  await expect(page.locator('#kbd-live')).toContainText(order[0] + ' ');
  await expect(page.locator('#kbd-live')).toContainText('1 of 3');
  await page.keyboard.press('j'); await page.keyboard.press('j'); await page.keyboard.press('j');
  expect(await current(page), 'j on the last row stays').toBe(order[2]);
  await page.keyboard.press('k'); await page.keyboard.press('k'); await page.keyboard.press('k');
  expect(await current(page), 'k on the first row stays').toBe(order[0]);
  await expect(page.locator('.row[aria-current=true]')).toBeInViewport();
  await page.keyboard.press('j');
  await page.keyboard.press('o');
  await expect(page).toHaveURL(new RegExp('#/issue/' + order[1] + '$'));
  await page.goBack();
  await expect(page.getByRole('listitem')).toHaveCount(3);
  await page.keyboard.press('j');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(new RegExp('#/issue/' + order[0] + '$'));
  await page.goBack();
  await expect(page.getByRole('listitem')).toHaveCount(3);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('/');
  await expect(page.locator('.bar input[type=search]')).toBeFocused();
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('c');
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await page.keyboard.press('g'); await page.keyboard.press('i');
  await expect(page).toHaveURL(/#\/inbox/);
  await page.keyboard.press('g'); await page.keyboard.press('b');
  await expect(page).toHaveURL(/#\/boards\//);
  await ctx.close();
});

test('KEY-02: shortcuts are inert while typing and while a dialog is open', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(page);
  const q = page.locator('.bar input[type=search]');
  await q.fill('');
  await q.pressSequentially('jkxeacg/');
  await expect(q).toHaveValue('jkxeacg/');
  await expect(page.locator('.row[aria-current=true]')).toHaveCount(0);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.goto('/#/issue/' + order[0]);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await page.getByPlaceholder('Reply…').pressSequentially('jk c?');
  await expect(page.getByPlaceholder('Reply…')).toHaveValue('jk c?');
  await page.getByLabel('Priority').focus();
  await page.keyboard.press('c');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByPlaceholder('Reply…').fill('');
  // a dialog is open: its own keys only
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('?');
  await expect(page.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeVisible();
  await page.keyboard.press('c');
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await ctx.close();
});

test('KEY-03, KEY-04: g then i within a second only; modifiers are left to the browser; the typed character counts', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  await listOf(page, 1);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('g');
  await page.waitForTimeout(1200);
  await page.keyboard.press('i');
  await expect(page).not.toHaveURL(/#\/inbox/);
  await page.keyboard.press('g'); await page.keyboard.press('z'); await page.keyboard.press('i');
  await expect(page).not.toHaveURL(/#\/inbox/);
  // nothing left pending: a later lone i does nothing either
  await page.keyboard.press('i');
  await expect(page).not.toHaveURL(/#\/inbox/);
  for (const chord of ['Control+c', 'Alt+c', 'Meta+c', 'Control+j']) {
    await page.keyboard.press(chord);
    await expect(page.getByRole('dialog'), chord + ' was swallowed').toHaveCount(0);
  }
  await page.keyboard.press('Shift+C'); // Caps Lock or Shift: the character is still c
  await expect(page.getByRole('dialog')).toBeVisible();
  await ctx.close();
});

test('KEY-05: e closes or resolves the current issue, says when there is no way to done, and tells a viewer why not', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(jana.page, 2);
  await jana.page.evaluate(() => document.activeElement && document.activeElement.blur());
  await jana.page.keyboard.press('j');
  await jana.page.keyboard.press('e'); // To Do has one transition into done (Resolve)
  await expect(jana.page.locator('#toasts')).toContainText(order[0] + ' moved to Done.');
  await expect.poll(async () => (await issue(jana.page, order[0])).status).toBe('Done');
  await jana.page.goto('/#/issue/' + order[0]);
  await expect(jana.page.getByLabel('Summary')).toBeVisible();
  await jana.page.evaluate(() => document.activeElement && document.activeElement.blur());
  await jana.page.keyboard.press('e'); // from Done there is only Reopen
  await expect(jana.page.locator('#toasts')).toContainText('has no transition to a done status from Done');
  const viewer = await newUser(browser, 'viewer@example.com');
  await viewer.page.goto('/#/issue/' + order[1]);
  await expect(viewer.page.getByLabel('Summary')).toBeVisible();
  await viewer.page.evaluate(() => document.activeElement && document.activeElement.blur());
  await viewer.page.keyboard.press('e');
  await expect(viewer.page.locator('#toasts')).toContainText('You can only view ' + order[1]);
  expect((await issue(viewer.page, order[1])).status).toBe('To Do');
  await jana.ctx.close(); await viewer.ctx.close();
});

test('KEY-06: a assigns with me first and a version check; a change by someone else in between is reported, not overwritten', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(page, 1);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('j');
  await page.keyboard.press('a');
  const dlg = page.getByRole('dialog', { name: 'Assign ' + order[0] });
  await expect(dlg.getByRole('option').first()).toContainText('Me');
  await expect(dlg.getByRole('option').nth(1)).toHaveText('Unassigned');
  await axe(page, 'assign picker');
  await page.keyboard.press('Enter');
  await expect(page.locator('#toasts')).toContainText(order[0] + ' assigned to you.');
  await expect.poll(async () => ((await issue(page, order[0])).assignee || {}).name).toBe('Jana K.');
  // type-ahead, and Escape cancels
  await expect(page.getByLabel('Select ' + order[0])).toBeVisible(); // the list is drawn again after the change
  await page.keyboard.press('j');
  await page.keyboard.press('a');
  await expect(dlg.getByRole('combobox')).toBeFocused();
  await page.keyboard.type('arka');
  await expect(dlg.getByRole('option')).toHaveCount(3); // Me and Unassigned stay, then the match
  await expect(dlg.getByRole('option').nth(2)).toHaveText('Arkady M.');
  await page.keyboard.press('Escape');
  await expect(dlg).toHaveCount(0);
  // someone else changes the issue while the picker is open
  await page.keyboard.press('j');
  await page.keyboard.press('a');
  await expect(dlg.getByRole('combobox')).toBeFocused();
  const other = await newUser(browser, 'arkady@example.com');
  await other.page.evaluate(async k => fetch('/api/v1/issues/' + k, { method: 'PATCH', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ priority: 'low' }) }), order[0]);
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter'); // Unassigned
  await expect(page.locator('#toasts')).toContainText('changed by someone else');
  expect(((await issue(page, order[0])).assignee || {}).name, 'the assignee was overwritten').toBe('Jana K.');
  await ctx.close(); await other.ctx.close();
});

test('KEY-07: j/k on the issue page walk the list it was opened from; opened from a link they do nothing', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(page);
  await page.getByRole('listitem').filter({ hasText: order[1] }).click();
  await expect(page).toHaveURL(new RegExp(order[1] + '$'));
  const loaded = async k => { await expect(page.locator('.key').first()).toHaveText(k); await expect(page.getByLabel('Summary')).toBeVisible(); };
  await loaded(order[1]);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('j');
  await expect(page).toHaveURL(new RegExp(order[2] + '$'));
  await loaded(order[2]);
  await page.keyboard.press('k');
  await loaded(order[1]);
  await page.keyboard.press('k');
  await expect(page).toHaveURL(new RegExp(order[0] + '$'));
  await loaded(order[0]);
  await page.keyboard.press('k'); // the first: stays
  await expect(page).toHaveURL(new RegExp(order[0] + '$'));
  const fresh = await ctx.newPage();
  await fresh.goto('/#/issue/' + order[1]);
  await expect(fresh.getByLabel('Summary')).toBeVisible();
  await fresh.evaluate(() => document.activeElement && document.activeElement.blur());
  await fresh.keyboard.press('j');
  await fresh.waitForTimeout(300);
  await expect(fresh).toHaveURL(new RegExp(order[1] + '$'));
  await ctx.close();
});

test('KEY-08: Ctrl+Enter and Cmd+Enter send a comment once and save the description; Enter alone is a new line', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'KEY-08 ' + tag(), 'first text');
  await page.goto('/#/issue/' + key);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  const reply = page.getByPlaceholder('Reply…');
  await reply.fill('line one');
  await reply.press('Enter');
  await reply.type('line two');
  await expect(reply).toHaveValue('line one\nline two');
  // a held key repeats: only one comment
  await reply.focus();
  await page.keyboard.down('Control');
  await page.keyboard.press('Enter'); await page.keyboard.press('Enter'); await page.keyboard.press('Enter');
  await page.keyboard.up('Control');
  await expect(page.getByText('line two')).toBeVisible();
  await page.waitForTimeout(500);
  const comments = await page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json()).comments.length, key);
  expect(comments).toBe(1);
  await page.getByPlaceholder('Reply…').fill('sent with Cmd');
  await page.getByPlaceholder('Reply…').press('Meta+Enter');
  await expect(page.getByText('sent with Cmd')).toBeVisible();
  await page.getByRole('tab', { name: 'Description' }).click();
  await page.getByRole('button', { name: 'Edit' }).click();
  await page.getByLabel('Description editor').fill('saved by keyboard');
  await page.getByLabel('Description editor').press('Control+Enter');
  await expect(page.getByLabel('Description editor')).toHaveCount(0);
  expect((await issue(page, key)).description).toBe('saved by keyboard');
  await ctx.close();
});

test('KEY-09: with single-key shortcuts off nothing fires, Ctrl+Enter still works, and the choice stays', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'arkady@example.com');
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('tab', { name: 'Keyboard' }).click();
  await page.getByLabel('Single-key shortcuts').uncheck();
  await expect(page.getByRole('dialog').getByRole('status')).toHaveText('Saved');
  await page.keyboard.press('Escape');
  const { order } = await listOf(page, 2);
  await page.reload();
  await expect(page.getByRole('listitem')).toHaveCount(2);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  for (const k of ['j', 'c', '/', '?', 'x']) await page.keyboard.press(k);
  await page.keyboard.press('g'); await page.keyboard.press('i');
  await expect(page.locator('.row[aria-current=true]')).toHaveCount(0);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page).not.toHaveURL(/inbox/);
  await page.goto('/#/issue/' + order[0]);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await page.getByPlaceholder('Reply…').fill('still sends');
  await page.getByPlaceholder('Reply…').press('Control+Enter');
  await expect(page.getByText('still sends')).toBeVisible();
  expect(await put(page, { shortcuts: true })).toBe(200);
  await ctx.close();
});

for (const theme of ['dark', 'light']) {
  test(`KEY-10: ? shows every registered shortcut grouped by screen, from the registry, and closes with Escape and ? (${theme})`, async ({ browser }) => {
    const { page, ctx } = await newUser(browser, 'jana@example.com');
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    await page.keyboard.press('?');
    const dlg = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
    await expect(dlg).toBeVisible();
    const reg = await page.evaluate(() => window.TLApp.shortcuts.map(d => ({ screen: d.screen, label: d.label })));
    expect(reg.length).toBeGreaterThan(10);
    for (const d of reg) {
      expect(d.label, 'a shortcut without a label').toBeTruthy();
      await expect(dlg.locator('section').filter({ has: page.getByRole('heading', { name: d.screen, exact: true }) }).getByRole('cell', { name: d.label, exact: true })).toBeVisible();
    }
    expect(await dlg.getByRole('row').count()).toBe(reg.length);
    // the registry refuses an entry without a label
    expect(await page.evaluate(() => { try { window.TLApp.registerShortcut({ keys: ['q'], screen: 'Anywhere', run() {} }); return 'accepted'; } catch (e) { return 'refused'; } })).toBe('refused');
    await axe(page, 'cheat sheet');
    await page.keyboard.press('Escape');
    await expect(dlg).toHaveCount(0);
    await page.keyboard.press('?');
    await expect(dlg).toBeVisible();
    await page.keyboard.press('?');
    await expect(dlg).toHaveCount(0);
    await ctx.close();
  });
}

test('KEY-01 x: x selects the current row like its checkbox, and the checkbox never opens the issue', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const { order } = await listOf(page, 2);
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.keyboard.press('j');
  await page.keyboard.press('x');
  await expect(page.getByLabel('Select ' + order[0])).toBeChecked();
  await expect(page.locator('#kbd-live')).toContainText('Selected ' + order[0] + ', 1 selected');
  await page.getByLabel('Select ' + order[1]).click();
  await expect(page).not.toHaveURL(/#\/issue\//);
  expect(await page.evaluate(() => [...window.TLApp.S.sel].sort())).toEqual([...order].sort());
  await page.keyboard.press('k'); // the clicked row became the current one
  await page.keyboard.press('x');
  await expect(page.getByLabel('Select ' + order[0])).not.toBeChecked();
  await ctx.close();
});
