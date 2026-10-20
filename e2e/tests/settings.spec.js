import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch, createIssue } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 7);

// UI-08: wide or narrow page, short or long rows, and the length of the description in the long view: kept on the server for each person
test('UI-08: view buttons and Settings are saved on the server and follow the person to another browser', async ({ browser }) => {
  const email = 'admin@example.com', pw = 'adminpass1';
  const ctx1 = await browser.newContext();
  ctx1.on('page', watch);
  const page = await ctx1.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(pw);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const tag = 'zq' + [...uniq()].map(c => 'abcdefghij'[parseInt(c, 36) % 10]).join('');
  const text = 'start-' + 'abcdefghij'.repeat(40) + '-END'; // 410 characters
  const made = await createIssue(page, 'Long view ' + tag, text);
  expect(made).toBeTruthy();
  await page.goto('/#/?q=' + encodeURIComponent('key = ' + made));
  const row = page.getByRole('listitem').filter({ hasText: 'Long view ' + tag });
  await expect(row).toBeVisible();
  // short by default: no description
  await expect(row.locator('.snip')).toHaveCount(0);
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['critical', 'serious'].includes(v.impact))).toEqual([]);
  // long view: the description appears
  await page.getByRole('button', { name: /^Long view/ }).click();
  await expect(row.locator('.snip')).toContainText('start-abcdefghij');
  await expect(row.locator('.snip')).toContainText('-END');
  // narrow: the page becomes a column
  const wide = await page.locator('.page.fluid').evaluate(e => e.getBoundingClientRect().width);
  await page.getByRole('button', { name: /^Narrow view/ }).click();
  await expect.poll(() => page.locator('.page.fluid').evaluate(e => e.getBoundingClientRect().width)).toBeLessThanOrEqual(900);
  expect(wide).toBeGreaterThan(900);
  // Settings: cut the description at 50 characters (each setting is saved as it changes)
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('tab', { name: 'List' }).click();
  await page.getByLabel('Description length in the long view').fill('50');
  await page.getByLabel('Description length in the long view').press('Tab');
  await expect(page.getByRole('dialog').getByRole('status')).toHaveText('Saved');
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(row.locator('.snip')).toContainText('start-abcdefghij');
  await expect(row.locator('.snip')).not.toContainText('-END');
  expect((await row.locator('.snip').innerText()).length).toBeLessThanOrEqual(51);
  // a bad value is refused by the server and nothing changes
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('tab', { name: 'List' }).click();
  await page.getByLabel('Description length in the long view').fill('0');
  await page.getByLabel('Description length in the long view').press('Tab');
  await expect(page.getByRole('dialog').getByRole('status')).toHaveText('Could not save the settings');
  await expect(page.getByLabel('Description length in the long view')).toHaveValue('50');
  await page.getByRole('button', { name: 'Done' }).click();
  // another browser, same person: the settings are there
  const ctx2 = await browser.newContext();
  const p2 = await ctx2.newPage();
  await p2.goto('/');
  await p2.getByLabel('Email').fill(email);
  await p2.getByLabel('Password').fill(pw);
  await p2.getByRole('button', { name: 'Sign in' }).click();
  await p2.goto('/#/?q=' + encodeURIComponent('key = ' + made));
  const r2 = p2.getByRole('listitem').filter({ hasText: 'Long view ' + tag });
  await expect(r2.locator('.snip')).toContainText('start-abcdefghij');
  await expect(p2.locator('body')).toHaveAttribute('data-width', 'narrow');
  expect(await p2.request.get('/api/v1/me/settings').then(r => r.json())).toEqual({ list_view: 'long', page_width: 'narrow', snippet_chars: 50 });
  // the issue page has the same width buttons and tabs for comments and history
  await p2.goto('/#/issue/' + (await r2.locator('.key').innerText()));
  await expect(p2.getByRole('tab', { name: /^Comments/ })).toBeVisible();
  await p2.getByRole('button', { name: /^Wide view/ }).click();
  await expect(p2.locator('body')).toHaveAttribute('data-width', 'wide');
  // put the shared account back to the defaults so no other test sees a long or narrow page
  await p2.evaluate(() => fetch('/api/v1/me/settings', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ list_view: 'short', page_width: 'wide', snippet_chars: 500 }) }));
  await ctx2.close();
  await ctx1.close();
});
