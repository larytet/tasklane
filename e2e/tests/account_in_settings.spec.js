import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, closeContexts } from './helpers.js';

// The account's sections (two-factor authentication, passkeys, password, sessions, connected applications, API tokens) are in Settings.

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

test('ACCT-SET-01: Settings (the gear) opens on Security, with the account sections in three tabs', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const dlg = page.getByRole('region', { name: 'Settings' });
  await expect(dlg).toBeVisible();
  await dlg.getByRole('tab', { name: 'Security' }).click();
  await expect(page.getByRole('button', { name: 'Account and security' })).toHaveCount(0); // the name button is gone: the gear is the one way in
  await expect(page).toHaveURL(/#\/settings\/security$/); // it is a screen of its own, with the tab in the address
  await expect(dlg.getByRole('tab', { name: 'Security' })).toHaveAttribute('aria-selected', 'true');
  for (const h of ['Two-factor authentication', 'Passkeys and security keys', 'Password']) await expect(dlg.getByRole('heading', { name: h })).toBeVisible();
  await expect(dlg.getByText('Security, sessions and two-factor')).toHaveCount(0); // the old link to the page is gone
  const ax = await new AxeBuilder({ page }).include('#settings-page').analyze();
  expect(ax.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);
  await dlg.getByRole('tab', { name: 'Sessions' }).click();
  await expect(dlg.getByRole('region', { name: 'Sessions' })).toBeVisible();
  await expect(dlg.getByText('(this session)')).toBeVisible();
  await dlg.getByRole('tab', { name: 'API tokens' }).click();
  await expect(dlg.getByRole('heading', { name: 'Connected applications' })).toBeVisible();
  await expect(dlg.getByRole('heading', { name: 'API tokens' })).toBeVisible();
  // the other settings are still there
  for (const t of ['Appearance', 'List', 'Keyboard']) await expect(dlg.getByRole('tab', { name: t })).toBeVisible();
});

test('ACCT-SET-02: a dialog opened from Settings sits above it, and Escape closes the dialog first and then leaves Settings', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('region', { name: 'Settings' }).getByRole('tab', { name: 'Security' }).click();
  await page.getByRole('button', { name: 'Set up two-factor authentication' }).click();
  const setup = page.getByRole('dialog', { name: 'Set up two-factor authentication' });
  await expect(setup).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(1); // Settings is hidden from assistive technology while the setup dialog is open
  await page.keyboard.press('Escape');
  await expect(setup).toBeHidden();
  const settings = page.getByRole('region', { name: 'Settings' });
  await expect(settings).toBeVisible(); // the first Escape closed only the top dialog
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden();
});

test('ACCT-SET-03: #/account still works for the flow that sends people there: it goes to Settings on Security, and is not a redirect loop', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.goto('/#/account');
  const dlg = page.getByRole('region', { name: 'Settings' });
  await expect(dlg).toBeVisible();
  await expect(dlg.getByRole('tab', { name: 'Security' })).toHaveAttribute('aria-selected', 'true');
  await expect(page).toHaveURL(/#\/settings\/security$/);
  await dlg.getByRole('button', { name: 'Done' }).click();
  await expect(dlg).toBeHidden();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('region', { name: 'Settings' })).toBeVisible();
});
