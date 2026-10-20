import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { createHmac } from 'node:crypto';

// the code an authenticator app shows for a base32 secret (RFC 6238, 30 s steps)
const totp = secret => {
  const b32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = ''; for (const c of secret.replace(/=+$/, '').toUpperCase()) bits += b32.indexOf(c).toString(2).padStart(5, '0');
  const kb = Buffer.from(bits.match(/.{8}/g).map(x => parseInt(x, 2)));
  const ctr = Buffer.alloc(8); ctr.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const h = createHmac('sha1', kb).update(ctr).digest(); const o = h[h.length - 1] & 0xf;
  return String(((h[o] & 0x7f) << 24 | h[o + 1] << 16 | h[o + 2] << 8 | h[o + 3]) % 1000000).padStart(6, '0');
};
import { pageErrors, watch, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

async function signIn(browser, email, password) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  return { ctx, page };
}

// SEC-04: an organization administrator requires a second factor in the Security tab; a member of that organization is then held at
// the Account page with the reason, adds an authenticator app, and is released. Another organization is not affected.
// (A separate organization, so the other tests, which share the Default one, are never held.)
test('SEC-04: requiring a second factor holds members of the organization to the enrolment page until they set one up', async ({ browser }) => {
  const tag = uniq();
  const adm = await signIn(browser, 'admin@example.com', 'adminpass1');
  await expect(adm.page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const org = await adm.ctx.request.fetch('/api/v1/orgs', { method: 'POST', headers: H, data: { name: 'Org ' + tag, admin_email: `boss${tag}@org.example`, admin_name: 'Org Boss', admin_password: 'password123' } });
  expect(org.status()).toBe(201);

  const boss = await signIn(browser, `boss${tag}@org.example`, 'password123');
  await expect(boss.page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const mk = await boss.ctx.request.fetch('/api/v1/users', { method: 'POST', headers: H, data: { email: `member${tag}@org.example`, name: 'Org Member', password: 'password123' } });
  expect(mk.status()).toBe(201);

  // requiring a second factor from everyone needs the administrator to have one first: they enrol through the API
  const setup = await (await boss.ctx.request.fetch('/api/v1/me/2fa/setup', { method: 'POST', headers: H })).json();
  expect((await boss.ctx.request.fetch('/api/v1/me/2fa/enable', { method: 'POST', headers: H, data: { code: totp(setup.secret) } })).status()).toBe(200);

  await boss.page.goto('/#/admin/security');
  await expect(boss.page.getByRole('heading', { name: 'Security policy' })).toBeVisible();
  await expect(boss.page.getByText(/no second factor yet/)).toBeVisible();
  const a11y = await new AxeBuilder({ page: boss.page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // a list that leaves the administrator out is refused (the e2e visitor comes from 127.0.0.1)
  await boss.page.getByLabel('Allowed networks').fill('203.0.113.0/24');
  await boss.page.getByRole('button', { name: 'Save policy' }).click();
  await expect(boss.page.getByText(/lock you out/)).toBeVisible();
  await boss.page.getByLabel('Allowed networks').fill('');
  // without one of their own the administrator would hold themselves: tested in the Go suite; here they have one
  await boss.page.getByLabel('Require a second factor').check();
  await boss.page.getByLabel('Longest session in hours').fill('12');
  await boss.page.getByLabel('Allowed email domains').fill('org.example');
  await boss.page.getByRole('button', { name: 'Save policy' }).click();
  await expect(boss.page.getByText('Policy saved')).toBeVisible();
  await expect(boss.page.getByLabel('Require a second factor')).toBeChecked();

  // an address outside the allowed domain cannot be given an account, in the words the database chose
  const outsider = await boss.ctx.request.fetch('/api/v1/users', { method: 'POST', headers: H, data: { email: `out${tag}@elsewhere.example`, name: 'Outsider', password: 'password123' } });
  expect(outsider.status()).toBe(400);
  expect((await outsider.json()).error).toContain('allowed domains');

  // the member signs in with the password and is taken to the account page, told why
  const mem = await signIn(browser, `member${tag}@org.example`, 'password123');
  await expect(mem.page.getByText(/Your organization requires a second factor/)).toBeVisible();
  await expect(mem.page).toHaveURL(/#\/account/);
  await expect(mem.page.getByRole('heading', { name: 'Two-factor authentication' })).toBeVisible();
  // everything else is refused
  const r = await mem.ctx.request.fetch('/api/v1/projects', { headers: H });
  expect(r.status()).toBe(403);
  expect((await r.json()).code).toBe('second_factor_required');
  // setting up an authenticator app releases them
  await mem.page.getByRole('button', { name: 'Set up two-factor authentication' }).click();
  const key = (await mem.page.getByLabel('Setup key').textContent()).trim();
  await mem.page.getByLabel('Code from the app').fill(totp(key));
  await mem.page.getByRole('button', { name: 'Turn on' }).click();
  await expect(mem.page.getByText('Your recovery codes')).toBeVisible();
  await mem.page.getByRole('button', { name: 'I have saved them' }).click();
  expect((await mem.ctx.request.fetch('/api/v1/projects', { headers: H })).status()).toBe(200);
});
