import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

async function admin(browser) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const r = await ctx.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } });
  expect(r.status()).toBe(200);
  return async (method, path, data) => {
    const res = await ctx.request.fetch(path, { method, headers: H, data });
    let body = null; try { body = await res.json(); } catch (e) { /* none */ }
    return { status: res.status(), body };
  };
}

test('custom fields: set in the create dialog, edited on the ticket, validated, accessible', async ({ browser }) => {
  const call = await admin(browser);
  const sev = 'Severity ' + uniq(), est = 'Estimate ' + uniq();
  const f1 = (await call('POST', '/api/v1/fields', { name: sev, type: 'select' })).body;
  await call('POST', `/api/v1/fields/${f1.id}/contexts`, { projects: ['ABC'], types: ['Story'], options: ['Low', 'High'], required: true });
  const f2 = (await call('POST', '/api/v1/fields', { name: est, type: 'duration' })).body;
  await call('POST', `/api/v1/fields/${f2.id}/contexts`, { projects: ['ABC'], types: ['Story'] });

  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: '+ Create' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Type').selectOption('Story');
  await dlg.getByLabel('Summary').fill('Has fields ' + uniq());
  await expect(dlg.getByLabel(sev)).toBeVisible();
  // a required field left empty is refused with the field named
  await dlg.getByRole('button', { name: 'Create' }).click();
  await expect(dlg.getByRole('alert')).toContainText(sev);
  await dlg.getByLabel(sev).selectOption('High');
  await dlg.getByLabel(est).fill('2h 30m');
  await dlg.getByRole('button', { name: 'Create' }).click();
  await expect(page.getByLabel(sev)).toHaveValue('High');
  // edit on the ticket
  await page.waitForLoadState('networkidle'); // the ticket re-renders as its panels load; edit once it is still
  await page.getByLabel(sev).selectOption('Low');
  await expect(page.getByLabel(sev)).toHaveValue('Low');
  await page.reload();
  await expect(page.getByLabel(sev)).toHaveValue('Low');
  await page.getByLabel(est).fill('nonsense');
  await page.getByLabel(est).blur();
  await expect(page.getByRole('alert').filter({ hasText: 'duration' })).toBeVisible();
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
});

test('behavior rules: a field becomes required and another is hidden as the values change', async ({ browser }) => {
  const call = await admin(browser);
  const why = 'Why ' + uniq(), note = 'Note ' + uniq();
  const f1 = (await call('POST', '/api/v1/fields', { name: why, type: 'text' })).body;
  await call('POST', `/api/v1/fields/${f1.id}/contexts`, { projects: ['ABC'], types: ['Epic'] });
  const f2 = (await call('POST', '/api/v1/fields', { name: note, type: 'text' })).body;
  await call('POST', `/api/v1/fields/${f2.id}/contexts`, { projects: ['ABC'], types: ['Epic'] });
  // highest priority requires a reason; a low priority hides the note
  await call('POST', '/api/v1/behavior-rules', { name: 'reason', project: 'ABC', type: 'Epic', when_field: 'priority', when_op: 'eq', when_value: 'highest', action: 'require', target: f1.key });
  await call('POST', '/api/v1/behavior-rules', { name: 'note', project: 'ABC', type: 'Epic', when_field: 'priority', when_op: 'in', when_value: ['low', 'lowest'], action: 'hide', target: f2.key });

  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: '+ Create' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Type').selectOption('Epic');
  await dlg.getByLabel('Summary').fill('Rules ' + uniq());
  await expect(dlg.getByLabel(why)).toHaveAttribute('aria-required', 'false');
  await dlg.getByLabel('Priority').selectOption('highest');
  await expect(dlg.getByLabel(why)).toHaveAttribute('aria-required', 'true');
  await expect(dlg.getByText(why + ' *')).toBeVisible();
  await dlg.getByRole('button', { name: 'Create' }).click();
  await expect(dlg.getByRole('alert')).toContainText(why);
  await dlg.getByLabel('Priority').selectOption('low');
  await expect(dlg.getByLabel(note)).toBeHidden();
  await expect(dlg.getByLabel(why)).toHaveAttribute('aria-required', 'false');
  await dlg.getByLabel('Priority').selectOption('medium');
  await expect(dlg.getByLabel(note)).toBeVisible();
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByLabel(note).fill('hello');
  await dlg.getByRole('button', { name: 'Create' }).click();
  await expect(page.getByLabel(note)).toHaveValue('hello');
});
