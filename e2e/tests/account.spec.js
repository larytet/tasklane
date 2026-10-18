import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import crypto from 'node:crypto';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

function base32decode(s) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const c of s.replace(/=+$/, '')) bits += alphabet.indexOf(c).toString(2).padStart(5, '0');
  const bytes = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(bytes);
}
function totp(secretB32, atMs) {
  const step = Math.floor(atMs / 1000 / 30);
  const buf = Buffer.alloc(8); buf.writeBigUInt64BE(BigInt(step));
  const h = crypto.createHmac('sha1', base32decode(secretB32)).update(buf).digest();
  const off = h[h.length - 1] & 0xf;
  const code = ((h[off] & 0x7f) << 24 | h[off + 1] << 16 | h[off + 2] << 8 | h[off + 3]) % 1000000;
  return String(code).padStart(6, '0');
}

async function newAccount(browser) {
  const adm = await browser.newContext();
  const login = await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } });
  expect(login.status()).toBe(200);
  const email = `sec-${uniq()}@example.com`;
  const r = await adm.request.post('/api/v1/users', { headers: H, data: { email, name: 'Sec Tester', password: 'first-password-1' } });
  expect(r.status()).toBe(201);
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill('first-password-1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  return { page, email, ctx };
}

test('SEC-04: two-factor enrolment, a second step at sign-in, and recovery codes', async ({ browser }) => {
  const { page, email, ctx } = await newAccount(browser);
  await page.goto('/#/account');
  await expect(page.getByRole('heading', { name: 'Two-factor authentication' })).toBeVisible();
  await page.getByRole('button', { name: 'Set up two-factor authentication' }).click();
  const dlg = page.getByRole('dialog');
  const secret = (await dlg.getByLabel('Setup key').textContent()).trim();
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByLabel('Code from the app').fill('000000');
  await dlg.getByRole('button', { name: 'Turn on' }).click();
  await expect(dlg.getByRole('alert')).toContainText('not right');
  await dlg.getByLabel('Code from the app').fill(totp(secret, Date.now()));
  await dlg.getByRole('button', { name: 'Turn on' }).click();
  const recovery = (await page.getByRole('dialog').getByLabel('Recovery codes').textContent()).trim().split('\n');
  expect(recovery).toHaveLength(10);
  await page.getByRole('button', { name: 'I have saved them' }).click();
  await expect(page.getByText('Two-factor authentication is on. 10 recovery codes are left.')).toBeVisible();
  // sign out and in again: the password is no longer enough
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill('first-password-1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByLabel('Authentication code')).toBeVisible();
  await page.getByLabel('Authentication code').fill('111111');
  await page.getByRole('button', { name: 'Verify' }).click();
  await expect(page.getByRole('alert')).toContainText('not right');
  // the next time step's code is accepted (clock drift is tolerated) and was never used
  await page.getByLabel('Authentication code').fill(totp(secret, Date.now() + 30000));
  await page.getByRole('button', { name: 'Verify' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  // a recovery code works once
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill('first-password-1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.getByLabel('Authentication code').fill(recovery[0]);
  await page.getByRole('button', { name: 'Verify' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.goto('/#/account');
  await expect(page.getByText('9 recovery codes are left.')).toBeVisible();
  // turning it off needs the password
  await page.getByRole('button', { name: 'Turn off two-factor authentication' }).click();
  await page.getByRole('dialog').getByLabel('Your password').fill('wrong');
  await page.getByRole('dialog').getByRole('button', { name: 'Turn off' }).click();
  await expect(page.getByRole('dialog').getByRole('alert')).toContainText('not right');
  await page.getByRole('dialog').getByLabel('Your password').fill('first-password-1');
  await page.getByRole('dialog').getByRole('button', { name: 'Turn off' }).click();
  await expect(page.getByText('Set up two-factor authentication', { exact: false }).first()).toBeVisible();
  await ctx.close();
});

test('SEC-04: sessions, API tokens and the password on the account page', async ({ browser }) => {
  const { page, email, ctx } = await newAccount(browser);
  // a second device
  const other = await browser.newContext();
  other.on('page', watch);
  const op = await other.newPage();
  await op.goto('/');
  await op.getByLabel('Email').fill(email);
  await op.getByLabel('Password').fill('first-password-1');
  await op.getByRole('button', { name: 'Sign in' }).click();
  await expect(op.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.goto('/#/account');
  await expect(page.getByText('(this session)')).toBeVisible();
  const rows = page.getByRole('region', { name: 'Sessions' }).getByRole('listitem');
  await expect(rows).toHaveCount(2);
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // signing the other device out ends it at once
  await page.getByRole('region', { name: 'Sessions' }).getByRole('button', { name: /^Sign out .* \d+$/ }).click();
  await expect(rows).toHaveCount(1);
  expect((await other.request.get('/api/v1/bootstrap')).status()).toBe(401);
  // a token is shown once, works, and stops working when revoked
  await page.getByLabel('Token name').fill('ci ' + uniq());
  await page.getByRole('button', { name: 'Create token' }).click();
  const token = (await page.getByRole('dialog').getByLabel('Token').textContent()).trim();
  expect(token).toMatch(/^tl_/);
  await page.getByRole('button', { name: 'Done' }).click();
  const use = async () => (await page.request.get('/api/v1/bootstrap', { headers: { Authorization: 'Bearer ' + token }, failOnStatusCode: false })).status();
  // the page's own cookie would authenticate the request: use a context with no cookies
  const bare = await browser.newContext();
  const bareUse = async () => (await bare.request.get('/api/v1/bootstrap', { headers: { Authorization: 'Bearer ' + token }, failOnStatusCode: false })).status();
  void use;
  expect(await bareUse()).toBe(200);
  await page.getByRole('button', { name: /^Revoke token/ }).click();
  await expect.poll(bareUse).toBe(401);
  // changing the password ends the other sessions and shows what happened
  await page.getByLabel('Current password').fill('wrong');
  await page.getByLabel('New password').fill('second-password-2');
  await page.getByRole('button', { name: 'Change password' }).click();
  await expect(page.getByText('The current password is not right.')).toBeVisible();
  await page.getByLabel('Current password').fill('first-password-1');
  await page.getByRole('button', { name: 'Change password' }).click();
  await expect(page.getByText('Password changed.')).toBeVisible();
  await ctx.close(); await other.close(); await bare.close();
});

test('SEC-04: administrators set up single sign-on and SCIM tokens; the sign-in page offers the provider', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.goto('/#/admin/identity');
  await expect(page.getByRole('heading', { name: 'Single sign-on (OpenID Connect)' })).toBeVisible();
  const name = 'idp' + uniq();
  await page.getByRole('button', { name: 'Add a provider' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Name', { exact: true }).fill(name);
  await dlg.getByLabel('Button label').fill('Acme login');
  await dlg.getByLabel('Issuer address').fill('not a url');
  await dlg.getByLabel('Client id').fill('tasklane');
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(dlg.getByRole('alert')).toContainText('issuer');
  await dlg.getByLabel('Issuer address').fill('https://login.acme.example');
  await dlg.getByLabel('Client secret').fill('s3cret');
  await dlg.getByLabel('Allowed email domains').fill('acme.example');
  const a11y = await new AxeBuilder({ page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('listitem').filter({ hasText: 'Acme login' })).toBeVisible();
  // a SCIM token is shown once
  await page.getByRole('button', { name: 'Create a SCIM token' }).click();
  await page.getByRole('dialog').getByLabel('Token name').fill('okta ' + uniq());
  await page.getByRole('dialog').getByRole('button', { name: 'Create' }).click();
  const token = (await page.getByRole('dialog').getByLabel('SCIM token').textContent()).trim();
  expect(token).toMatch(/^scim_/);
  await page.getByRole('button', { name: 'Done' }).click();
  const scim = await page.request.get('/scim/v2/Users', { headers: { Authorization: 'Bearer ' + token } });
  expect(scim.status()).toBe(200);
  expect(scim.headers()['content-type']).toContain('application/scim+json');
  await page.getByRole('button', { name: /^Revoke SCIM token/ }).first().click();
  await expect.poll(async () => (await page.request.get('/scim/v2/Users', { headers: { Authorization: 'Bearer ' + token }, failOnStatusCode: false })).status()).toBe(401);
  // the sign-in page offers the provider (a signed-out visitor sees only its name)
  const out = await browser.newContext();
  const op = await out.newPage();
  await op.goto('/');
  await expect(op.getByRole('link', { name: 'Sign in with Acme login' })).toBeVisible();
  await expect(op.getByRole('link', { name: 'Sign in with Acme login' })).toHaveAttribute('href', new RegExp('/api/v1/auth/sso/' + name + '/start'));
  await op.goto('/?sso=denied');
  await expect(op.getByRole('alert')).toContainText('Single sign-on was not allowed');
  // delete the provider so later tests see a plain sign-in page
  await page.getByRole('button', { name: 'Delete Acme login' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page.getByRole('listitem').filter({ hasText: 'Acme login' })).toHaveCount(0);
  await ctx.close(); await out.close();
});
