import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const view = (page, key) => page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json()), key);
const toasts = page => page.locator('#toasts');
const undoBtn = page => toasts(page).getByRole('button', { name: 'Undo' });
// the Undo of the toast that says text (several can be on the screen at once)
const undoOf = (page, text) => toasts(page).locator('.toast').filter({ hasText: text }).getByRole('button', { name: 'Undo' });
async function resolveBySelect(page, key) {
  await page.goto('/#/issue/' + key);
  const sel = page.getByLabel('Transition');
  await expect(sel).toBeEnabled();
  const resolve = await sel.locator('option', { hasText: 'Resolve' }).getAttribute('value');
  await sel.selectOption(resolve);
  await expect(toasts(page)).toContainText(key + ' moved to Done.');
}

test('UNDO-01: a transition, an archive and a delete each offer Undo, and Undo puts the issue back', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'UNDO-01 ' + Date.now(), 'x');
  await resolveBySelect(page, key);
  await undoOf(page, 'moved to Done').click();
  await expect(toasts(page)).toContainText('Undone.');
  await expect(page.locator('.loz').first()).toHaveText('To Do');
  expect((await view(page, key)).issue.status).toBe('To Do');
  // archive (it has to be done first)
  await resolveBySelect(page, key);
  await page.getByRole('button', { name: 'Actions ▾' }).click();
  await page.getByRole('button', { name: 'Archive', exact: true }).click();
  await expect(toasts(page)).toContainText(key + ' archived.');
  await undoOf(page, 'archived').click();
  await expect(toasts(page)).toContainText('Undone.');
  expect((await view(page, key)).issue.archived).toBeFalsy();
  // delete: back from the trash, with its key, on its page
  await page.getByRole('button', { name: 'Actions ▾' }).click();
  await page.getByRole('button', { name: 'Delete…' }).click();
  await expect(toasts(page)).toContainText(key + ' moved to the trash.');
  await undoOf(page, 'moved to the trash').click();
  await expect(page).toHaveURL(new RegExp('#/issue/' + key + '$'));
  await expect(page.getByLabel('Summary')).toBeVisible();
  // history says what happened, with who did it
  const hist = await page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/history', { credentials: 'same-origin' })).json()), key);
  const status = hist.filter(x => x.field === 'status').map(x => x.before + '>' + x.after);
  expect(status).toEqual(['To Do>Done', 'Done>To Do', 'To Do>Done']);
  expect(hist.filter(x => x.field === 'restored').length).toBe(1);
  await ctx.close();
});

test('UNDO-02: the toast goes after 5 s, stays while hovered or focused, toasts stack, and it is said once', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const a = await createIssue(page, 'UNDO-02 a', 'x');
  await resolveBySelect(page, a);
  await expect(undoBtn(page)).toBeVisible();
  await page.waitForTimeout(4000);
  await expect(undoBtn(page)).toBeVisible();
  await expect(undoBtn(page)).toHaveCount(0, { timeout: 2500 }); // gone at 5 s
  // hovered: it stays past 5 s and goes once the pointer leaves
  const b = await createIssue(page, 'UNDO-02 b', 'x');
  await resolveBySelect(page, b);
  await toasts(page).locator('.toast').filter({ hasText: b }).hover();
  await page.waitForTimeout(6500);
  await expect(undoBtn(page)).toBeVisible();
  await page.mouse.move(5, 5);
  await expect(undoBtn(page)).toHaveCount(0, { timeout: 6000 });
  // focused: the same with the keyboard
  const c = await createIssue(page, 'UNDO-02 c', 'x');
  await resolveBySelect(page, c);
  await undoBtn(page).focus();
  await page.waitForTimeout(6500);
  await expect(undoBtn(page)).toBeVisible();
  // a second one stacks under it; the region is polite and the toast is in it once
  const d = await createIssue(page, 'UNDO-02 d', 'x');
  await page.goto('/#/issue/' + d);
  const sel = page.getByLabel('Transition');
  await sel.selectOption(await sel.locator('option', { hasText: 'Resolve' }).getAttribute('value'));
  await expect(toasts(page).locator('.toast').filter({ hasText: d + ' moved to Done.' })).toHaveCount(1);
  await expect(toasts(page)).toHaveAttribute('aria-live', 'polite');
  const r = await new AxeBuilder({ page }).include('#toasts').analyze();
  expect(r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical')).toEqual([]);
  await ctx.close();
});

test('UNDO-04, UNDO-07: a change by someone else in between is reported and nothing is reverted; another viewer sees both changes', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'UNDO-04 ' + Date.now(), 'x');
  await ark.page.goto('/#/issue/' + key);
  await expect(ark.page.locator('.loz').first()).toHaveText('To Do');
  await resolveBySelect(jana.page, key);
  await expect(ark.page.locator('.loz').first()).toHaveText('Done', { timeout: 10000 }); // realtime, or the page's own poll
  await undoBtn(jana.page).hover(); // keep it while the other person writes
  await ark.page.evaluate(async k => fetch('/api/v1/issues/' + k + '/comments', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ body: 'meanwhile' }) }), key);
  await undoBtn(jana.page).click();
  await expect(toasts(jana.page)).toContainText('has a new comment since');
  expect((await view(jana.page, key)).issue.status).toBe('Done');
  // a clean one: undone, and the other viewer sees it go back without reloading
  const k2 = await createIssue(jana.page, 'UNDO-07 ' + Date.now(), 'x');
  await ark.page.goto('/#/issue/' + k2);
  await expect(ark.page.locator('.loz').first()).toHaveText('To Do');
  await resolveBySelect(jana.page, k2);
  await expect(ark.page.locator('.loz').first()).toHaveText('Done', { timeout: 10000 });
  await undoBtn(jana.page).click();
  await expect(toasts(jana.page)).toContainText('Undone.');
  await expect(ark.page.locator('.loz').first()).toHaveText('To Do', { timeout: 10000 });
  await jana.ctx.close(); await ark.ctx.close();
});

test('UNDO-03 over HTTP: a token only when asked for, single use, and nobody else\'s', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'UNDO-03 ' + Date.now(), 'x');
  const go = (page, want) => page.evaluate(async ({ k, want }) => {
    const v = await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json();
    const t = v.transitions.find(x => x.to_category === 'done') || v.transitions[0];
    const h = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    if (want) h['Tasklane-Want-Undo'] = '1';
    const r = await fetch('/api/v1/issues/' + k + '/transitions', { method: 'POST', credentials: 'same-origin', headers: h, body: JSON.stringify({ transition: t.id }) });
    return r.headers.get('Tasklane-Undo');
  }, { k: key, want });
  const undo = (page, tok) => page.evaluate(async t => (await fetch('/api/v1/undo/' + t, { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } })).status, tok);
  expect(await go(jana.page, false)).toBeNull();
  await undo(jana.page, 'x'); // nothing to undo
  const reopen = await jana.page.evaluate(async k => { const v = await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json(); return v.transitions.find(x => x.name === 'Reopen').id; }, key);
  await jana.page.evaluate(async ({ k, id }) => fetch('/api/v1/issues/' + k + '/transitions', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ transition: id }) }), { k: key, id: reopen });
  const tok = await go(jana.page, true);
  expect(tok).toMatch(/^un_/);
  expect(await undo(ark.page, tok), 'another person').toBe(404);
  const tab2 = await jana.ctx.newPage(); // the same person in another tab
  await tab2.goto('/');
  expect(await undo(tab2, tok)).toBe(200);
  expect(await undo(jana.page, tok), 'used twice').toBe(404);
  await jana.ctx.close(); await ark.ctx.close();
});
