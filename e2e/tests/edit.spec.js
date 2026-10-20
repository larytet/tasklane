import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const view = (page, key) => page.evaluate(async k => (await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json()), key);
const editor = page => page.getByLabel('Description editor');

test('EDIT-01: the description opens where it is by a click; Escape closes it and keeps the draft; Ctrl+Enter and leaving it save', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'EDIT-01 ' + Date.now(), 'first words');
  await page.goto('/#/issue/' + key);
  await expect(page.getByRole('button', { name: /^Edit$/ })).toHaveCount(0); // no Edit button: the text is the control
  await page.getByText('first words').click();
  await expect(editor(page)).toBeFocused();
  await editor(page).fill('typed, then Escape');
  await page.keyboard.press('Escape');
  await expect(editor(page)).toHaveCount(0);
  expect((await view(page, key)).issue.description).toBe('first words'); // nothing saved
  await expect(page.getByText(/Restored your unsaved draft/)).toBeVisible(); // and nothing lost
  await page.getByRole('button', { name: 'Continue editing' }).click();
  await expect(editor(page)).toHaveValue('typed, then Escape');
  await editor(page).fill('saved with Ctrl+Enter');
  await editor(page).press('Control+Enter');
  await expect(editor(page)).toHaveCount(0);
  expect((await view(page, key)).issue.description).toBe('saved with Ctrl+Enter');
  // leaving the field saves
  await page.getByText('saved with Ctrl+Enter').click();
  await editor(page).fill('saved by leaving');
  await page.getByLabel('Summary').focus();
  await expect(editor(page)).toHaveCount(0);
  expect((await view(page, key)).issue.description).toBe('saved by leaving');
  // an empty description is a place to click too
  const empty = await createIssue(page, 'EDIT-01 empty ' + Date.now(), '');
  await page.goto('/#/issue/' + empty);
  await page.getByText('No description. Click to add one.').click();
  await expect(editor(page)).toBeVisible();
});

test('EDIT-01: the in-place save keeps the conflict rules: another person\'s save in between opens the conflict dialog', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'EDIT-01 conflict ' + Date.now(), 'Steps:\n\nExpected: a');
  for (const u of [jana, ark]) { await u.page.goto('/#/issue/' + key); await u.page.getByText('Expected: a').click(); }
  await jana.page.getByLabel('Description editor').fill('Steps:\n\nExpected: b');
  await jana.page.getByLabel('Description editor').press('Control+Enter');
  await expect(jana.page.getByText('Expected: b')).toBeVisible();
  await ark.page.getByLabel('Description editor').fill('Steps:\n\nExpected: c');
  await ark.page.getByLabel('Description editor').press('Control+Enter');
  await expect(ark.page.getByRole('dialog')).toBeVisible();
});

test('EDIT-02: custom fields edit in place; a required one cannot be emptied', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const admin = await ctx.newPage();
  await admin.goto('/');
  await admin.getByLabel('Email').fill('admin@example.com');
  await admin.getByLabel('Password').fill('adminpass1');
  await admin.getByRole('button', { name: 'Sign in' }).click();
  await expect(admin.getByRole('button', { name: 'Create new' })).toBeVisible();
  const name = 'Team ' + Date.now();
  const f = await admin.evaluate(async name => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    const r = await (await fetch('/api/v1/fields', { method: 'POST', headers: H, body: JSON.stringify({ name, type: 'text' }) })).json();
    // in XYZ only, so a required field cannot get in the way of the other tests, which work in ABC
    await fetch('/api/v1/fields/' + r.id + '/contexts', { method: 'POST', headers: H, body: JSON.stringify({ projects: ['XYZ'], required: true, default: 'core' }) });
    return r;
  }, name);
  try {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await page.evaluate(async s => (await (await fetch('/api/v1/issues', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ project: 'XYZ', type: 'Task', summary: s }) })).json()).key, 'EDIT-02 ' + Date.now());
  await page.goto('/#/issue/' + key);
  const fld = page.getByLabel(name);
  await expect(fld).toHaveValue('core');
  await fld.fill('payments');
  await fld.press('Tab');
  await expect.poll(async () => (await view(page, key)).issue.custom[f.key]).toBe('payments');
  await fld.fill('');
  await fld.press('Tab');
  await expect(page.locator('.err[role=alert]').filter({ hasText: /required/i })).toBeVisible();
  expect((await view(page, key)).issue.custom[f.key]).toBe('payments');
  } finally {
    await admin.evaluate(async id => fetch('/api/v1/fields/' + id, { method: 'DELETE', credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } }), f.id);
    await ctx.close();
  }
});

test('EDIT-03: nothing becomes editable for a viewer or on an archived issue, and the reason is shown', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'EDIT-03 ' + Date.now(), 'read me');
  const vera = await newUser(browser, 'viewer@example.com');
  await vera.page.goto('/#/issue/' + key);
  await expect(vera.page.getByRole('note')).toContainText('You can view this project but not change its issues.');
  await expect(vera.page.getByLabel('Summary')).toHaveAttribute('readonly', '');
  await expect(vera.page.getByRole('button', { name: 'Edit description' })).toHaveCount(0);
  await vera.page.getByText('read me').click();
  await expect(vera.page.getByLabel('Description editor')).toHaveCount(0);
  // archived
  const v = await view(jana.page, key);
  await jana.page.evaluate(async ({ k, id }) => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    await fetch('/api/v1/issues/' + k + '/transitions', { method: 'POST', headers: H, body: JSON.stringify({ transition: id }) });
    await fetch('/api/v1/issues/' + k + '/archive', { method: 'POST', headers: H });
  }, { k: key, id: v.transitions.find(t => t.to_category === 'done').id });
  await jana.page.goto('/#/issue/' + key);
  await expect(jana.page.getByRole('note')).toContainText('archived and read-only');
  await jana.page.getByText('read me').click();
  await expect(jana.page.getByLabel('Description editor')).toHaveCount(0);
});

for (const theme of ['dark', 'light']) {
  test(`EDIT-04: every in-place field by keyboard: Tab to it, Enter opens, Escape closes; the summary's Escape puts back the saved text (${theme})`, async ({ browser }) => {
    const { page } = await newUser(browser, 'jana@example.com');
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    const key = await createIssue(page, 'EDIT-04 ' + theme, 'by keyboard');
    await page.goto('/#/issue/' + key);
    await expect(page.getByLabel('Summary')).toBeVisible();
    await page.getByLabel('Summary').focus();
    await page.keyboard.type(' changed');
    await page.keyboard.press('Escape');
    await expect(page.getByLabel('Summary')).toHaveValue('EDIT-04 ' + theme);
    expect((await view(page, key)).issue.summary).toBe('EDIT-04 ' + theme);
    // Tab reaches the description's pencil
    let reached = false;
    for (let i = 0; i < 40 && !reached; i++) {
      await page.keyboard.press('Tab');
      reached = await page.evaluate(() => document.activeElement && document.activeElement.getAttribute('aria-label') === 'Edit description');
    }
    expect(reached, 'the description is reachable by Tab').toBe(true);
    await page.keyboard.press('Enter');
    await expect(editor(page)).toBeFocused();
    const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
    expect(r.violations.filter(x => ['serious', 'critical'].includes(x.impact)).map(x => x.id)).toEqual([]);
    await page.keyboard.press('Escape');
    await expect(editor(page)).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Edit description' })).toBeVisible();
  });
}
