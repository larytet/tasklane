import { test, expect } from '@playwright/test';
import { pageErrors, newUser, closeContexts, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

test('ADM-SET-01: the administration tabs are in Settings, not a button in the top bar', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Admin', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const dlg = page.getByRole('region', { name: 'Settings' });
  const admin = page.getByRole('navigation', { name: 'Administration' });
  for (const t of ['Workflows', 'Automation', 'Identity', 'Email', 'Git', 'Jira', 'Roles', 'Security', 'Audit log']) await expect(admin.getByRole('link', { name: t, exact: true })).toBeVisible();
  await expect(dlg.getByText('Open administration')).toHaveCount(0);
  await admin.getByRole('link', { name: 'Identity' }).click();
  await expect(page).toHaveURL(/#\/admin\/identity$/);
  await page.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'Settings' }).click();
  await expect(page).toHaveURL(/#\/settings$/);
  await ctx.close();
});

test('ADM-SET-02: a person who is not an administrator has no administration tabs', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('region', { name: 'Settings' })).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Administration' })).toHaveCount(0);
});
