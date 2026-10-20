import { test, expect } from '@playwright/test';
import { pageErrors, newUser, createIssue, saveAsApi, login, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

test('LIVE-11: a draft survives a killed tab and is restored with a banner; a changed base opens the conflict dialog', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'Draft test', 'original text');
  await jana.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await jana.page.getByLabel('Description editor').fill('unsaved words that must not be lost');
  await jana.page.waitForTimeout(2500); // the local draft is written every 2 s
  await jana.page.close({ runBeforeUnload: false }); // the tab dies without saving
  const page = await jana.ctx.newPage();
  await page.goto(`/#/issue/${key}`);
  await expect(page.getByText(/Restored your unsaved draft/)).toBeVisible();
  // meanwhile somebody else saved: the draft's base is stale, so saving opens the dialog
  const ark = await newUser(browser, 'arkady@example.com');
  await ark.page.goto('/');
  expect((await saveAsApi(ark.page, key, 'changed by Arkady meanwhile', 1)).status).toBe(200);
  await page.getByRole('button', { name: 'Continue editing' }).click();
  await expect(page.getByLabel('Description editor')).toHaveValue('unsaved words that must not be lost');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByRole('dialog')).toContainText('Arkady M. saved changes');
});

test('LIVE-11: discarding a restored draft removes it for good', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'Discard test', 'original');
  await jana.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await jana.page.getByLabel('Description editor').fill('throw me away');
  await jana.page.waitForTimeout(2500);
  await jana.page.close({ runBeforeUnload: false });
  const page = await jana.ctx.newPage();
  await page.goto(`/#/issue/${key}`);
  await page.getByRole('button', { name: 'Discard' }).click();
  await expect(page.getByText(/Restored your unsaved draft/)).toHaveCount(0);
  await page.reload();
  await expect(page.getByText(/Restored your unsaved draft/)).toHaveCount(0);
});

test('PRES-01: the other person sees who is here and who is editing which field, and it clears', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Presence', 'text');
  await jana.page.goto(`/#/issue/${key}`);
  await ark.page.goto(`/#/issue/${key}`);
  await expect(jana.page.getByText('Arkady M. is here')).toBeVisible();
  await expect(jana.page.getByLabel('1 others here')).toBeVisible(); // names are text, never colour alone
  await ark.page.getByRole('button', { name: 'Edit' }).click();
  await expect(jana.page.getByText(/Arkady M\. is editing the description/)).toBeVisible();
  // Jana opening the same field gets the gentle warning, and can continue
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await expect(jana.page.getByText(/Arkady M\. is also editing this field/)).toBeVisible();
  await expect(jana.page.getByLabel('Description editor')).toBeEnabled();
  await ark.page.getByRole('button', { name: 'Cancel' }).click();
  await expect(jana.page.getByText(/Arkady M\. is also editing/)).toHaveCount(0);
  await ark.page.goto('/'); // leaving the issue
  await expect(jana.page.getByText('Arkady M. is here')).toHaveCount(0);
});

test('PRES-04: a viewer sees who is editing but cannot open an editor or save', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const vera = await newUser(browser, 'viewer@example.com');
  const key = await createIssue(jana.page, 'Viewer test', 'text');
  await jana.page.goto(`/#/issue/${key}`);
  await vera.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await expect(vera.page.getByText(/Jana K\. is editing the description/)).toBeVisible();
  await expect(vera.page.getByRole('button', { name: 'Edit' })).toHaveCount(0);
  await vera.page.getByRole('tab', { name: /^Comments/ }).click();
  await expect(vera.page.getByText('You have read-only access')).toBeVisible();
  await expect(vera.page.getByLabel('Priority')).toBeDisabled();
});

test('live updates: a change by someone else appears on an open page without any action', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Live', 'text');
  await jana.page.goto(`/#/issue/${key}`);
  await ark.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('tab', { name: /^Comments/ }).click();
  await ark.page.getByRole('tab', { name: /^Comments/ }).click();
  await ark.page.getByPlaceholder('Reply…').fill('hello from Arkady');
  await ark.page.getByRole('button', { name: 'Send' }).click();
  await expect(jana.page.getByText('hello from Arkady')).toBeVisible();
  await ark.page.getByLabel('Priority').selectOption('highest');
  await expect(jana.page.getByLabel('Priority')).toHaveValue('highest');
});

test('PERF/realtime fallback: with the socket blocked the page still works and catches up by polling', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'No socket', 'text');
  const ctx = await browser.newContext();
  await ctx.route('**/api/v1/ws**', r => r.abort());
  const page = await ctx.newPage();
  await login(page, 'arkady@example.com');
  await page.goto(`/#/issue/${key}`);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await page.getByPlaceholder('Reply…').fill('works without a socket');
  await page.getByRole('button', { name: 'Send' }).click();
  await expect(page.getByText('works without a socket')).toBeVisible();
  await jana.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('tab', { name: /^Comments/ }).click();
  await jana.page.getByPlaceholder('Reply…').fill('seen by polling');
  await jana.page.getByRole('button', { name: 'Send' }).click();
  await expect(page.getByText('seen by polling')).toBeVisible({ timeout: 12_000 }); // the 4 s fallback poll
});
