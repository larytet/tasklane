import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

// The newest message to an address in the mail sink: the code and the link.
async function sentTo(addr, notBefore = 0) {
  for (let i = 0; i < 60; i++) {
    const dir = process.env.E2E_MAIL_DIR;
    const files = readdirSync(dir).sort().filter(f => f > String(notBefore).padStart(6, '0') + '.eml');
    for (const f of files.reverse()) {
      const raw = readFileSync(join(dir, f), 'utf8').replace(/=\r?\n/g, '').replace(/=([0-9A-F]{2})/g, (m, h) => String.fromCharCode(parseInt(h, 16)));
      if (raw.toLowerCase().includes('<' + addr.toLowerCase() + '>') && raw.includes('sign-in code')) {
        const code = raw.match(/is (\d{6})/)[1];
        const token = raw.match(/token=([A-Za-z0-9_-]+)/)[1];
        return { code, token, n: Number(f.slice(0, 6)) };
      }
    }
    await new Promise(r => setTimeout(r, 250));
  }
  throw new Error('no sign-in mail to ' + addr);
}

test('PORT-01: a customer signs in with an emailed code, sets a name, and a staff session cannot open the portal', async ({ browser }) => {
  // an administrator opens a portal on a project of its own
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'P' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Help desk' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true, welcome: 'How can we help you today?', privacy_notice: 'We keep your address to answer you.', consent_text: 'By signing in you agree to be contacted about your requests.' } })).status()).toBe(200);

  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  await expect(page.getByRole('heading', { name: 'Help desk', level: 1 })).toBeVisible();
  await expect(page.getByText('How can we help you today?')).toBeVisible();
  await expect(page.getByText('By signing in you agree')).toBeVisible();
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);

  const addr = `cora.${uniq()}@customer.example`;
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  await expect(page.getByText('a code is on its way')).toBeVisible();
  const { code } = await sentTo(addr);
  // a wrong code says so, in the same words however it is wrong
  await page.getByLabel('The six digit code we sent you').fill('000000');
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(page.getByRole('alert')).toContainText('not right');
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(page.getByText('Signed in as')).toContainText(addr);
  // the profile
  await page.getByLabel('Name').fill('Cora <b>C.</b>');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Saved.')).toBeVisible();
  await page.reload();
  await expect(page.getByLabel('Name')).toHaveValue('Cora <b>C.</b>');
  // the customer's session opens nothing of the staff side, and the staff app does not know them
  const staff = await ctx.request.get('/api/v1/me');
  expect(staff.status()).toBe(401);
  expect((await ctx.request.get('/api/v1/projects/' + pk + '/portal')).status()).toBe(401);
  // a staff session opens nothing of the portal
  const stf = await adm.request.get('/api/v1/portal/me');
  expect(stf.status()).toBe(401);
  // signing out ends it
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  expect((await ctx.request.get('/api/v1/portal/me')).status()).toBe(401);
  // the emailed link signs in once, and the token leaves the address bar
  await page.getByLabel('Your email address').fill(addr);
  const before = (await sentTo(addr)).n;
  await page.getByRole('button', { name: 'Email me a code' }).click();
  const second = await sentTo(addr, before);
  await page.goto(`/portal/${pk}?token=${second.token}`);
  await expect(page.getByText('Signed in as')).toContainText(addr);
  expect(page.url()).not.toContain('token=');
  const again = await ctx.newPage();
  await again.context().clearCookies();
  await again.goto(`/portal/${pk}?token=${second.token}`);
  await expect(again.getByRole('alert')).toContainText('used or has expired');
});
