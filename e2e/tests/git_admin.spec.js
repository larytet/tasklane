import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 6).toUpperCase();

// GIT-UI-ADMIN: an administrator manages the Git integration in the Git tab: connection, repository, visibility, linked
// account, smart commits and a dry-run rule, and sees the rule in the list.
test('GIT: the Git tab manages connections, visibility, linked accounts and rules', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  const pk = 'G' + uniq();
  const r = await ctx.request.fetch('/api/v1/projects', { method: 'POST', headers: H, data: { key: pk, name: 'Git admin test' } });
  expect(r.status()).toBe(201);

  await page.goto('/#/admin/git');
  await expect(page.getByRole('heading', { name: 'Git', exact: true })).toBeVisible();
  const name = 'gh-' + uniq();
  await page.getByLabel('Connection name').fill(name);
  await page.getByLabel('Access token').fill('ghp_e2e_not_a_real_token');
  await page.getByRole('button', { name: 'Add connection' }).click();
  await expect(page.getByLabel('Webhook secret')).toBeVisible();
  await page.getByRole('button', { name: 'Done' }).click();
  const conn = page.getByRole('list', { name: 'Connections' }).getByRole('listitem').filter({ hasText: name });
  await expect(conn).toContainText('github');
  await expect(conn).not.toContainText('ghp_e2e'); // the token is never shown, only its fingerprint

  await page.getByLabel('Repository', { exact: true }).fill('acme/' + name);
  await page.getByLabel('Projects to link').fill(pk);
  await page.getByRole('button', { name: 'Map repository' }).click();
  await expect(page.getByRole('list', { name: 'Repositories' })).toContainText('acme/' + name);

  await page.getByLabel('Who sees development data').selectOption('restricted');
  await expect(page.getByText('Visibility saved')).toBeVisible();
  await expect(page.getByText('privacy unknown (treated as private)').first()).toBeVisible();
  await page.getByRole('button', { name: 'Treat acme/' + name + ' as public' }).click();
  await expect(page.getByText(/· public ·|public · 0 with access/).first()).toBeVisible();
  await page.getByLabel('Who sees development data').selectOption('default');

  const login = 'octo' + uniq().toLowerCase();
  await page.getByLabel('Provider account').fill(login);
  await page.getByLabel('Person').selectOption({ index: 0 });
  await page.getByRole('button', { name: 'Link account' }).click();
  await expect(page.getByRole('list', { name: 'Linked accounts' })).toContainText(login);
  await page.getByRole('button', { name: 'Unlink ' + login }).click();
  await expect(page.getByRole('list', { name: 'Linked accounts' })).not.toContainText(login);

  await page.getByLabel('Project for rules and smart commits').selectOption(pk);
  await page.getByLabel('Smart commits', { exact: true }).selectOption('default_branch');
  await expect(page.getByText('Smart commits saved')).toBeVisible();
  await page.getByLabel('Rule name').fill('Start on branch');
  await page.getByLabel('Trigger').selectOption('branch_created');
  await page.getByLabel('Transition').fill('Start progress');
  await page.getByRole('button', { name: 'Add rule (dry run)' }).click();
  const rule = page.getByRole('list', { name: 'Rules' }).getByRole('listitem').filter({ hasText: 'Start on branch' });
  await expect(rule).toContainText('branch created');
  await rule.getByLabel('Mode of rule Start on branch').selectOption('on');
  await expect(rule.getByLabel('Mode of rule Start on branch')).toHaveValue('on');
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await rule.getByRole('button', { name: 'Delete rule Start on branch' }).click();
  await expect(page.getByRole('list', { name: 'Rules' })).not.toContainText('Start on branch');
});
