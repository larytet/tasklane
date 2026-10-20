import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

async function withComment(page, text) {
  const key = await createIssue(page, 'REACT ' + Date.now(), 'x');
  await page.evaluate(async ({ k, t }) => fetch('/api/v1/issues/' + k + '/comments', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ body: t }) }), { k: key, t: text });
  return key;
}
const msg = (page, text) => page.locator('.msg').filter({ hasText: text });

for (const theme of ['dark', 'light']) {
  test(`REACT-06: the picker by keyboard, counts and who reacted announced, axe clean (${theme})`, async ({ browser }) => {
    const { page } = await newUser(browser, 'jana@example.com');
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    const key = await withComment(page, 'react to me ' + theme);
    await page.goto('/#/issue/' + key);
    await page.getByRole('tab', { name: /^Comments/ }).click();
    const m = msg(page, 'react to me ' + theme);
    await m.getByRole('button', { name: 'Add reaction' }).focus();
    await page.keyboard.press('Enter');
    await expect(m.getByRole('group', { name: 'Choose a reaction' })).toBeVisible();
    await expect(m.getByRole('button', { name: 'React with 👍' })).toBeFocused();
    await page.keyboard.press('ArrowRight'); await page.keyboard.press('ArrowRight'); await page.keyboard.press('ArrowRight'); // 🎉
    await expect(m.getByRole('button', { name: 'React with 🎉' })).toBeFocused();
    const r = await new AxeBuilder({ page }).include('.msg').analyze();
    expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);
    await page.keyboard.press('Enter');
    const tada = m.getByRole('button', { name: /^🎉 1 reaction by Jana K\./ });
    await expect(tada).toHaveAttribute('aria-pressed', 'true');
    await expect(tada).toHaveAttribute('title', 'Jana K.');
    await expect(tada).toBeFocused(); // focus stays on the reaction
    await page.keyboard.press('Enter'); // take it back
    await expect(m.getByRole('button', { name: /^🎉/ })).toHaveCount(0);
    // Escape closes the picker and gives focus back
    await m.getByRole('button', { name: 'Add reaction' }).click();
    await page.keyboard.press('Escape');
    await expect(m.getByRole('group', { name: 'Choose a reaction' })).toBeHidden();
    await expect(m.getByRole('button', { name: 'Add reaction' })).toBeFocused();
  });
}

test('REACT-03: two people react at once; both are kept and each sees the other\'s live', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await withComment(jana.page, 'together');
  for (const u of [jana, ark]) { await u.page.goto('/#/issue/' + key); await u.page.getByRole('tab', { name: /^Comments/ }).click(); }
  const both = [jana, ark].map(u => u.page.evaluate(async k => {
    const v = await (await fetch('/api/v1/issues/' + k + '/view', { credentials: 'same-origin' })).json();
    return (await fetch('/api/v1/issues/' + k + '/comments/' + v.comments[0].id + '/reactions', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ emoji: '👍', on: true }) })).status;
  }, key));
  expect(await Promise.all(both)).toEqual([200, 200]);
  for (const u of [jana, ark]) await expect(msg(u.page, 'together').getByRole('button', { name: /^👍 2 reactions by/ })).toBeVisible({ timeout: 5000 });
  // one more by Arkady from the page: Jana sees it without reloading
  await msg(ark.page, 'together').getByRole('button', { name: 'Add reaction' }).click();
  await msg(ark.page, 'together').getByRole('button', { name: 'React with 🚀' }).click();
  await expect(msg(jana.page, 'together').getByRole('button', { name: /^🚀 1 reaction by Arkady M\./ })).toBeVisible({ timeout: 5000 });
});
