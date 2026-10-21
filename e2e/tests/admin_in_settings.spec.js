import { test, expect } from '@playwright/test';
import { pageErrors, newUser, closeContexts, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

test('ADM-SET-01: administration is a tab in Settings, not a button in the top bar', async ({ browser }) => {
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
  const dlg = page.getByRole('dialog', { name: 'Settings' });
  await dlg.getByRole('tab', { name: 'Administration' }).click();
  await dlg.getByRole('link', { name: 'Open administration' }).click();
  await expect(dlg).toBeHidden();
  await expect(page).toHaveURL(/#\/admin\//);
  await ctx.close();
});

test('ADM-SET-02: a person who is not an administrator has no Administration tab', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Settings' }).getByRole('tab', { name: 'Administration' })).toHaveCount(0);
});
