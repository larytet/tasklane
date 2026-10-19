import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 6);

// SEC-04 (LDAP): the administrator's form. The directory itself is exercised by the Go tests against an in-process directory (`ldapmock`); here the
// browser's part: the form asks for what is missing, saves a provider, lists it as LDAP, offers no button for it on the sign-in page (people type their
// directory name into the ordinary form), and "Try a sign-in" reports a directory that cannot be reached instead of failing silently.
test('SEC-04: an administrator adds an LDAP provider in the form, tries a sign-in, and the sign-in page offers no button for it', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.goto('/#/admin/identity');
  const name = 'ldap' + uniq();
  await page.getByRole('button', { name: 'Add a provider' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Protocol').selectOption('ldap');
  await dlg.getByLabel('Name', { exact: true }).fill(name);
  await dlg.getByLabel('Button label').fill('Corp directory');
  // nothing about the directory entered: the reason is given
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(dlg.getByRole('alert')).toBeVisible();
  await dlg.getByLabel('Server addresses').fill('ldap://127.0.0.1:1');
  await dlg.getByLabel('Search account DN').fill('cn=search,dc=example,dc=com');
  await dlg.getByLabel('Search account password').fill('search-secret');
  await dlg.getByLabel('Base DN').fill('ou=people,dc=example,dc=com');
  await dlg.getByLabel('User filter').fill('(uid={user})');
  await dlg.getByLabel('LDAP email attribute').fill('mail');
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Save' }).click();
  const row = page.getByRole('listitem').filter({ hasText: 'Corp directory' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('LDAP');
  await expect(row).toContainText('ldap://127.0.0.1:1');
  // the password is never shown again
  await row.getByRole('button', { name: 'Edit Corp directory' }).click();
  const edit = page.getByRole('dialog');
  await expect(edit.getByLabel('Search account password')).toHaveValue('');
  await expect(edit.getByLabel('Search account password')).toHaveAttribute('placeholder', 'unchanged');
  // try a sign-in against a directory nobody is running: an answer, not silence
  await edit.getByLabel('Test name').fill('jana');
  await edit.getByLabel('Test password').fill('whatever');
  await edit.getByRole('button', { name: 'Try a sign-in' }).click();
  const answer = edit.getByRole('status').filter({ hasText: /\S/ }).first();
  await expect(answer).toContainText(/Refused|not allowed|could not|failed/i, { timeout: 15000 }); // here: the egress policy refuses a loopback directory
  await edit.getByRole('button', { name: 'Cancel' }).click();

  // a signed-out visitor is not offered a button for it
  const out = await browser.newContext();
  const op = await out.newPage();
  await op.goto('/');
  await expect(op.getByLabel('Email')).toBeVisible();
  await expect(op.getByRole('link', { name: /Corp directory/ })).toHaveCount(0);
  await expect(op.getByRole('button', { name: /Corp directory/ })).toHaveCount(0);
});
