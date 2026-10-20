import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

test('PORT: an administrator makes a queue and a canned response; an agent works the queue and inserts the response into a reply', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const call = async (method, path, data) => { const r = await ctx.request.fetch(path, { method, headers: H, data }); let b = null; try { b = await r.json(); } catch (e) { /* none */ } return { status: r.status(), body: b }; };
  const pk = 'Q' + uniq().toUpperCase();
  expect((await call('POST', '/api/v1/projects', { key: pk, name: 'Queues' })).status).toBe(201);
  const mine = (await call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Waiting for us', priority: 'high' })).body.key;
  await call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Low thing', priority: 'low' });

  // the top bar's More menu reaches the page, and passes the accessibility check while open
  await page.getByText('More', { exact: true }).click();
  const axeMenu = await new AxeBuilder({ page }).analyze();
  expect(axeMenu.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await page.getByRole('link', { name: 'Queues', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Queues' })).toBeVisible();
  await page.goto('/#/queues/' + pk);
  await expect(page.getByRole('heading', { name: 'Queues' })).toBeVisible();
  await expect(page.getByText('No queues yet.')).toBeVisible();
  await page.getByLabel('Queue name').fill('Urgent');
  await page.getByLabel('Queue query').fill('priority = high');
  await page.getByRole('button', { name: 'Add queue' }).click();
  await expect(page.getByRole('link', { name: 'Urgent' })).toBeVisible();
  await expect(page.getByRole('table').getByRole('link', { name: mine })).toBeVisible();
  await expect(page.getByText('1 issue(s)')).toBeVisible();
  // a query that does not compile is refused with the reason
  await page.getByLabel('Queue name').fill('Broken');
  await page.getByLabel('Queue query').fill('priority =');
  await page.getByRole('button', { name: 'Add queue' }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'not valid' })).toBeVisible();
  // a canned response
  await page.getByLabel('Canned response name').fill('Thanks');
  await page.getByLabel('Canned response text').fill('Hello {{customer}}, we are on {{issue.key}}. {{agent}}');
  await page.getByRole('button', { name: 'Add canned response' }).click();
  await expect(page.getByRole('list', { name: 'Canned responses' })).toContainText('Thanks');
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);

  // on the ticket, the response is inserted with the page's own words filled in
  await page.goto('/#/issue/' + mine);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await page.getByLabel('Insert a canned response').selectOption({ label: 'Thanks' });
  await expect(page.getByLabel('Reply', { exact: true })).toHaveValue('Hello there, we are on ' + mine + '. Administrator');
});
