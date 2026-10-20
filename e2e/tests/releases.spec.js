import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 7);

test('CORE-12: versions: add, put an issue in one, see progress, release with a warning, move the open work on, read the notes', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const p = jana.page;
  const key = await createIssue(p, 'Release me ' + uniq(), 'for a version');
  const project = key.split('-')[0];
  // Jana runs the project's releases: an administrator makes her a project administrator
  const adm = await browser.newContext();
  const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const me = await p.evaluate(async () => (await fetch('/api/v1/me', { credentials: 'same-origin' })).json());
  expect((await adm.request.post('/api/v1/projects/' + project + '/members', { headers: H, data: { user: me.id, role: 'admin' } })).status()).toBe(200);
  const v1 = 'v' + uniq(), v2 = 'v' + uniq();
  await p.goto('/#/releases/' + project);
  await expect(p.getByRole('heading', { name: 'Releases' })).toBeVisible();
  for (const v of [v1, v2]) {
    await p.getByLabel('Version name').fill(v);
    await p.getByRole('button', { name: 'Add version' }).click();
    await expect(p.getByRole('listitem').filter({ hasText: v })).toBeVisible();
  }
  // the issue gets the first version from its own screen
  await p.goto('/#/issue/' + key);
  const fix = p.getByLabel('Fix versions');
  await expect(fix).toBeVisible();
  await fix.fill(v1);
  await fix.press('Enter');
  await fix.blur();
  await expect(p.getByLabel('Fix versions')).toHaveValue(v1);
  // progress shows one issue, none done
  await p.goto('/#/releases/' + project);
  const row1 = p.getByRole('listitem').filter({ hasText: v1 });
  await expect(row1.getByRole('img', { name: /0 of 1 done/ })).toBeVisible();
  const a11y = await new AxeBuilder({ page: p }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // releasing warns about the unfinished issue; moving it on releases the version
  await row1.getByRole('button', { name: 'Release ' + v1 }).click();
  await p.getByRole('dialog').getByRole('button', { name: 'Release', exact: true }).click();
  await expect(p.getByRole('dialog').getByRole('alert')).toContainText('1 issue(s)');
  await expect(p.getByRole('dialog').getByRole('link', { name: key })).toBeVisible();
  await p.getByLabel('Move unfinished issues to').selectOption({ label: v2 });
  await p.getByRole('button', { name: 'Move and release' }).click();
  await expect(p.getByRole('listitem').filter({ hasText: v1 }).getByText('Released', { exact: true })).toBeVisible();
  await expect(p.getByRole('listitem').filter({ hasText: v2 }).getByRole('img', { name: /0 of 1 done/ })).toBeVisible();
  // the notes name what is not done, as text
  await p.getByRole('listitem').filter({ hasText: v2 }).getByRole('button', { name: 'Notes' }).click();
  await expect(p.getByLabel('Release notes text')).toContainText('Not done');
  await expect(p.getByLabel('Release notes text')).toContainText(key);
});
