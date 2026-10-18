import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 6).toUpperCase();

async function adminPage(browser) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  const call = async (method, path, data) => {
    const res = await ctx.request.fetch(path, { method, headers: H, data });
    let body = null; try { body = await res.json(); } catch (e) { /* none */ }
    return { status: res.status(), body };
  };
  return { page, call };
}

test('LIFE-03: edit a workflow in the editor, publish, and remove a status that holds issues', async ({ browser }) => {
  const { page, call } = await adminPage(browser);
  const key = 'W' + uniq(), name = 'Flow ' + uniq();
  const wf = (await call('POST', '/api/v1/workflows', { name, template: 'software' })).body;
  expect((await call('POST', '/api/v1/projects', { key, name: 'Workflow test' })).status).toBe(201);
  expect((await call('PUT', `/api/v1/projects/${key}/workflow`, { workflow: wf.id })).status).toBe(200);
  const keys = [];
  for (let i = 0; i < 3; i++) keys.push((await call('POST', '/api/v1/issues', { project: key, type: 'Task', summary: 'in flight ' + i })).body.key);
  const view = async k => (await call('GET', `/api/v1/issues/${k}/view`)).body;
  for (const k of keys.slice(0, 2)) {
    let v = await view(k);
    await call('POST', `/api/v1/issues/${k}/transitions`, { transition: v.transitions.find(t => t.name === 'Start progress').id });
    v = await view(k);
    await call('POST', `/api/v1/issues/${k}/transitions`, { transition: v.transitions.find(t => t.name === 'Request review').id });
  }
  expect((await view(keys[0])).issue.status).toBe('In Review');

  // the list shows it, the editor opens it
  await page.goto('/#/admin/workflows');
  await page.getByRole('listitem').filter({ hasText: name }).click();
  await expect(page.getByRole('heading', { name })).toBeVisible();
  await expect(page.getByRole('img', { name: /Workflow diagram: 4 statuses/ })).toBeVisible();
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // the live workflow is read-only until a draft is started
  await expect(page.getByLabel('Status name').first()).toBeDisabled();
  await page.getByRole('button', { name: 'Edit workflow' }).click();
  await page.getByRole('button', { name: 'Add status' }).click();
  await page.getByLabel('Status name').last().fill('Blocked');
  await expect(page.getByText('Unsaved changes')).toBeVisible();
  await page.getByRole('button', { name: 'Remove In Review' }).click();
  await expect(page.getByRole('img', { name: /Workflow diagram: 4 statuses/ })).toBeVisible(); // +1 added, -1 removed
  await page.getByRole('button', { name: 'Check and publish' }).click();
  const dlg = page.getByRole('dialog');
  // two issues are in the removed status: publishing waits until they have somewhere to go
  await expect(dlg).toContainText('2 issue(s) are in In Review');
  await expect(dlg.getByRole('button', { name: 'Publish' })).toBeDisabled();
  await dlg.getByLabel('Move the 2 issues in In Review to').selectOption('In Progress');
  await expect(dlg.getByRole('button', { name: 'Publish' })).toBeEnabled();
  const a11y2 = await new AxeBuilder({ page }).analyze();
  expect(a11y2.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Publish' }).click();
  await expect(page.getByText(/Published version 2/)).toBeVisible();
  // the background job moves the issues; both end up in In Progress and the retired status is gone
  await expect.poll(async () => (await view(keys[0])).issue.status, { timeout: 20000 }).toBe('In Progress');
  expect((await view(keys[1])).issue.status).toBe('In Progress');
  const live = (await call('GET', `/api/v1/workflows/${wf.id}`)).body;
  expect(live.live.statuses.map(s => s.name)).toContain('Blocked');
  expect(live.live.statuses.map(s => s.name)).not.toContain('In Review');
  expect(live.version).toBe(2);
});
