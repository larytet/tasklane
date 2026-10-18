import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 6).toUpperCase();

// PERM-02: an administrator makes a custom role in the Roles tab, gives it to a person in a project, and the person can do what the
// role says and nothing else; the role in use cannot be deleted.
test('PERM-02: a custom role is made in the Roles tab and gives exactly what it names', async ({ browser }) => {
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
  const tag = uniq();
  const role = 'Reporter ' + tag;
  await page.goto('/#/admin/roles');
  await expect(page.getByRole('heading', { name: 'Roles', exact: true })).toBeVisible();
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await page.getByLabel('Role name').fill(role);
  await page.getByLabel('Grants: Create issues').check();
  await page.getByLabel('Grants: Comment').check();
  await page.getByRole('button', { name: 'Add role' }).click();
  const row = page.getByRole('list', { name: 'Roles' }).getByRole('listitem').filter({ hasText: role });
  await expect(row).toContainText('a viewer who may also: comment, create');
  // a reserved name is refused with the reason
  await page.getByLabel('Role name').fill('Admin');
  await page.getByRole('button', { name: 'Add role' }).click();
  await expect(page.getByText(/is a built-in role/)).toBeVisible();

  // give it to a person in a new project
  const pk = 'R' + tag;
  expect((await call('POST', '/api/v1/projects', { key: pk, name: 'Roles test' })).status).toBe(201);
  const email = `rep${tag.toLowerCase()}@example.com`;
  const u = await call('POST', '/api/v1/users', { email, name: 'Rita Reporter', password: 'password123' });
  expect(u.status).toBe(201);
  expect((await call('POST', `/api/v1/projects/${pk}/members`, { user: u.body.id, role })).status).toBe(200);
  const issue = (await call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Existing issue' })).body.key;

  const rep = await browser.newContext();
  const login = await rep.request.fetch('/api/v1/login', { method: 'POST', headers: H, data: { email, password: 'password123' } });
  expect(login.status()).toBe(200);
  const as = async (method, path, data) => { const r = await rep.request.fetch(path, { method, headers: H, data }); return r.status(); };
  expect(await as('GET', `/api/v1/issues/${issue}/view`)).toBe(200);
  expect(await as('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Made by the reporter' })).toBe(201);
  expect(await as('POST', `/api/v1/issues/${issue}/comments`, { body: 'A comment by the reporter' })).toBeLessThan(300);
  expect(await as('PATCH', `/api/v1/issues/${issue}`, { priority: 'high' })).toBe(403); // not an editor
  const list = (await call('GET', '/api/v1/roles')).body;
  const mine = list.find(r => r.name === role);
  expect(mine.in_use).toBe(1);

  // in use: not deletable
  await page.reload();
  await row.getByRole('button', { name: 'Delete ' + role }).click();
  await expect(page.getByText(/still given to people/)).toBeVisible();
  // take the comment permission away in the tab: the reporter loses it at once
  await row.getByRole('button', { name: 'Edit ' + role }).click();
  await page.getByLabel('Grants: Comment').first().uncheck();
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByRole('list', { name: 'Roles' }).getByRole('listitem').filter({ hasText: role })).toContainText('a viewer who may also: create');
  expect(await as('POST', `/api/v1/issues/${issue}/comments`, { body: 'Another comment' })).toBe(403);
  expect(await as('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Still allowed to create' })).toBe(201);
});
