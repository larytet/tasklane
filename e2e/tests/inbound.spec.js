import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { createHmac } from 'node:crypto';
import { pageErrors, watch, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 6).toUpperCase();

// MAIL-07, MAIL-09, MAIL-21: a message delivered by webhook waits in quarantine, an administrator reviews and releases it,
// and the ticket shows the comment as coming from email.
test('MAIL-21: review and release a quarantined email in the Email tab', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const call = async (method, path, data) => {
    const res = await ctx.request.fetch(path, { method, headers: H, data });
    let body = null; try { body = await res.json(); } catch (e) { /* none */ }
    return { status: res.status(), body };
  };
  const pk = 'M' + uniq();
  expect((await call('POST', '/api/v1/projects', { key: pk, name: 'Mail test' })).status).toBe(201);
  const issue = (await call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Mail target' })).body.key;
  const src = await call('POST', '/api/v1/admin/mail/sources', { name: 'e2e ' + uniq(), trusted: true });
  expect(src.status).toBe(201);
  // an unproven message from the administrator, naming the issue in its subject
  const raw = `From: admin@example.com\r\nTo: tasklane@example.com\r\nSubject: Re: [${issue}] Mail target\r\nMessage-ID: <${uniq()}@e2e.example>\r\nContent-Type: text/plain\r\n\r\nThis came by email.\r\n`;
  const sig = 'sha256=' + createHmac('sha256', src.body.secret).update(raw).digest('hex');
  const res = await ctx.request.fetch(`/api/v1/mail/inbound/${src.body.id}`, { method: 'POST', headers: { 'X-Tasklane-Signature': sig }, data: raw });
  expect(res.status()).toBe(200);
  expect((await res.json()).outcome).toBe('quarantined');

  await page.goto('/#/admin/email');
  await expect(page.getByRole('heading', { name: 'Inbound email' })).toBeVisible();
  const row = page.getByRole('listitem').filter({ hasText: 'proof of origin' }).first();
  await expect(row).toContainText('admin@example.com');
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await row.getByRole('button', { name: /^Release message/ }).click();
  await expect(page.getByText('Released as a comment')).toBeVisible();

  await page.goto(`/#/issue/${issue}`);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await expect(page.getByText('This came by email.')).toBeVisible();
  await expect(page.getByText('via email')).toBeVisible();
});

// MAIL-09: a mailbox is added in the Email tab, its failure is visible to the administrator, and it can be removed.
test('MAIL-09: add an IMAP mailbox in the Email tab, see its state, remove it', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await page.goto('/#/admin/email');
  const name = 'Box ' + uniq();
  await page.getByLabel('Mailbox name').fill(name);
  await page.getByLabel('IMAP host').fill('imap.invalid');
  await page.getByLabel('IMAP port').fill('993');
  await page.getByLabel('IMAP user').fill('support');
  await page.getByLabel('IMAP password').fill('not-a-real-password');
  await page.getByRole('button', { name: 'Add mailbox' }).click();
  const row = page.getByRole('listitem').filter({ hasText: name });
  await expect(row).toContainText('support@imap.invalid:993');
  // the password is never shown back
  await expect(page.getByText('not-a-real-password')).toHaveCount(0);
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await row.getByRole('button', { name: /^Check mailbox/ }).click();
  await expect(page.getByText(/imap|resolve|egress|connect/i).first()).toBeVisible();
  await row.getByRole('button', { name: /^Delete mailbox/ }).click();
  await expect(page.getByRole('listitem').filter({ hasText: name })).toHaveCount(0);
});
