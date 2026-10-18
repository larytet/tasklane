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

test('SEC-07: the audit log shows administrative changes, filters, verifies and exports', async ({ browser }) => {
  const { page, call } = await adminPage(browser);
  const name = 'Audited flow ' + uniq();
  expect((await call('POST', '/api/v1/workflows', { name, template: 'simple' })).status).toBe(201);
  await page.goto('/#/admin/audit');
  await expect(page.getByRole('heading', { name: 'Audit log' })).toBeVisible();
  await expect(page.getByRole('listitem').filter({ hasText: 'POST /api/v1/workflows' }).first()).toBeVisible();
  await page.getByLabel('Filter by action').fill('workflow.create');
  await page.getByLabel('Filter by action').press('Tab');
  await expect(page.getByRole('listitem').filter({ hasText: 'workflow.create' }).first()).toBeVisible();
  await expect(page.getByRole('listitem').filter({ hasText: 'POST /api/v1/workflows' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Verify the chain' }).click();
  await expect(page.getByText(/The chain is intact/)).toBeVisible();
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  const res = await page.request.get('/api/v1/admin/audit/export?action=workflow.');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('text/csv');
  expect(await res.text()).toContain('workflow.create');
});

test('CORE-10: an automation rule is created in the admin screen, dry-run, runs on an event and logs it', async ({ browser }) => {
  const { page, call } = await adminPage(browser);
  const key = 'A' + uniq();
  expect((await call('POST', '/api/v1/projects', { key, name: 'Automation test' })).status).toBe(201);
  await page.goto('/#/admin/automation/' + key);
  await expect(page.getByRole('heading', { name: 'Automation' })).toBeVisible();
  await page.getByRole('button', { name: 'New rule' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Rule name').fill('Say thanks');
  await dlg.getByLabel('Trigger').fill(JSON.stringify({ type: 'issue.created' }));
  await dlg.getByLabel('Actions').fill(JSON.stringify([{ kind: 'comment', text: 'Thanks for reporting {{issue.key}}' }]));
  await dlg.getByLabel('Enabled').check();
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('listitem').filter({ hasText: 'Say thanks' })).toBeVisible();
  // a bad rule is refused with the reason
  await page.getByRole('button', { name: 'New rule' }).click();
  await page.getByRole('dialog').getByLabel('Actions').fill(JSON.stringify([{ kind: 'format_disk' }]));
  await page.getByRole('dialog').getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('dialog').getByRole('alert')).toContainText('unknown action');
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();
  // an issue is created: the rule comments, as the automation
  const created = (await call('POST', '/api/v1/issues', { project: key, type: 'Task', summary: 'automated' })).body;
  await expect.poll(async () => {
    const v = (await call('GET', `/api/v1/issues/${created.key}/view`)).body;
    return v.comments.map(c => c.body).join('|');
  }, { timeout: 20000 }).toContain('Thanks for reporting ' + created.key);
  // dry run and the run log
  await page.getByRole('button', { name: 'Dry run Say thanks' }).click();
  await page.getByRole('dialog').getByLabel('Issue key').fill(created.key);
  await page.getByRole('dialog').getByRole('button', { name: 'Show what it would do' }).click();
  await expect(page.getByRole('dialog')).toContainText('comment: Thanks for reporting ' + created.key);
  await page.getByRole('dialog').getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Runs of Say thanks' }).click();
  await expect(page.getByRole('dialog')).toContainText(created.key + ' · done');
  await page.getByRole('dialog').getByRole('button', { name: 'Close' }).click();
  // disabling stops it
  await page.getByLabel('Enable Say thanks').uncheck();
  await page.waitForTimeout(300);
  const second = (await call('POST', '/api/v1/issues', { project: key, type: 'Task', summary: 'quiet' })).body;
  await new Promise(r => setTimeout(r, 3500));
  const v2 = (await call('GET', `/api/v1/issues/${second.key}/view`)).body;
  expect(v2.comments.length).toBe(0);
});
