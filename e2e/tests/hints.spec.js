import { test, expect } from '@playwright/test';
import { pageErrors, closeContexts, watch, PASSWORD } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

// a new person, who has seen no hint yet (the seeded ones have seen them all)
async function freshPerson(browser) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const email = 'hint' + Date.now() + Math.random().toString(36).slice(2, 6) + '@example.com';
  await page.evaluate(async email => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    const u = await (await fetch('/api/v1/users', { method: 'POST', headers: H, body: JSON.stringify({ email, name: 'New Person', password: 'password123' }) })).json();
    await fetch('/api/v1/projects/ABC/members', { method: 'POST', headers: H, body: JSON.stringify({ user: u.id, role: 'member' }) });
  }, email);
  await ctx.close();
  return email;
}
async function signIn(browser, email, opts = {}) {
  const ctx = await browser.newContext(opts);
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  return { ctx, page };
}
const hint = page => page.getByRole('note').filter({ has: page.getByRole('button', { name: 'Got it' }) });

test('HINT-01: one hint each for c, the view buttons and Settings, at most one per session, never again on another device', async ({ browser }) => {
  const email = await freshPerson(browser);
  const a = await signIn(browser, email);
  await expect(hint(a.page)).toContainText('Press c to create');
  const focused = await a.page.evaluate(() => document.activeElement && document.activeElement.tagName);
  expect(focused, 'the hint took focus').not.toBe('BUTTON');
  await hint(a.page).getByRole('button', { name: 'Got it' }).click();
  await expect(hint(a.page)).toHaveCount(0);
  await a.page.reload(); // the same session: no second hint
  await expect(a.page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await a.page.waitForTimeout(2500);
  await expect(hint(a.page)).toHaveCount(0);
  await a.ctx.close();
  const b = await signIn(browser, email); // another device: the next hint, and the first never again
  await expect(hint(b.page)).toContainText('Short or long rows');
  await b.ctx.close();
  const c = await signIn(browser, email);
  await expect(hint(c.page)).toContainText('Your settings');
  await c.ctx.close();
  const d = await signIn(browser, email);
  await d.page.waitForTimeout(2500);
  await expect(hint(d.page)).toHaveCount(0); // all three seen
  await d.ctx.close();
});

test('HINT-02: a hint never covers a dialog and keyboard work goes on; no keyboard hint on touch', async ({ browser }) => {
  const email = await freshPerson(browser);
  const a = await signIn(browser, email);
  await expect(hint(a.page)).toBeVisible();
  await a.page.keyboard.press('c'); // typing goes on: c still opens the dialog, and the hint goes away
  await expect(a.page.getByRole('dialog')).toBeVisible();
  await expect(hint(a.page)).toHaveCount(0);
  await a.ctx.close();
  const touchEmail = await freshPerson(browser);
  const t = await signIn(browser, touchEmail, { hasTouch: true, isMobile: true, viewport: { width: 390, height: 800 } });
  await expect(hint(t.page)).toBeVisible();
  await expect(hint(t.page)).not.toContainText('Press c');
  await t.ctx.close();
});

test('HINT-03: with shortcuts off the c hint is not shown', async ({ browser }) => {
  const email = await freshPerson(browser);
  const a = await signIn(browser, email);
  await a.page.evaluate(async () => fetch('/api/v1/me/ui-prefs', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ shortcuts: false, hints_seen: [] }) }));
  await a.ctx.close();
  const b = await signIn(browser, email);
  await expect(hint(b.page)).toBeVisible();
  await expect(hint(b.page)).not.toContainText('Press c');
  await b.ctx.close();
});
