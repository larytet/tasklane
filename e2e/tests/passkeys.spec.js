import { test, expect } from '@playwright/test';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);
// Chrome does not accept an IP address as a relying party id, so the passkey test uses the host name localhost; the server is told
// (webauthn_rp_id, webauthn_origins) in global-setup.
const LOCAL = 'http://localhost:18181';

// SEC-04: register a passkey on the account page, sign in with it as the second step after the password, remove it.
test('SEC-04: a passkey is added on the account page, finishes a sign-in after the password, and can be removed', async ({ browser }) => {
  // a person of their own, so that no other test finds a second step waiting for it
  const adm = await browser.newContext();
  const ap = await adm.newPage();
  await ap.goto('/');
  await ap.getByLabel('Email').fill('admin@example.com');
  await ap.getByLabel('Password').fill('adminpass1');
  await ap.getByRole('button', { name: 'Sign in' }).click();
  await expect(ap.getByRole('button', { name: 'Create new' })).toBeVisible();
  const email = `pk${uniq()}@example.com`;
  const mk = await adm.request.fetch('/api/v1/users', { method: 'POST', headers: H, data: { email, name: 'Passkey P.', password: 'password123' } });
  expect(mk.status()).toBe(201);

  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } });
  const signIn = async () => {
    await page.goto(LOCAL + '/');
    await page.getByLabel('Email').fill(email);
    await page.getByLabel('Password').fill('password123');
    await page.getByRole('button', { name: 'Sign in' }).click();
  };
  await signIn();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible(); // no key yet: the password is enough

  await page.goto(LOCAL + '/#/account');
  await expect(page.getByRole('heading', { name: 'Passkeys and security keys' })).toBeVisible();
  await expect(page.getByText('No passkey or security key is registered.')).toBeVisible();
  await page.getByRole('button', { name: 'Add a passkey or security key' }).click();
  await page.getByLabel('Name for the key').fill('e2e key');
  // the wrong password does not start a registration
  await page.getByLabel('Your password').fill('wrong');
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(page.getByText('That password is not right.')).toBeVisible();
  await page.getByLabel('Your password').fill('password123');
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(page.getByText('Key added', { exact: true })).toBeVisible();
  const row = page.getByRole('list', { name: 'Passkeys' }).getByRole('listitem').filter({ hasText: 'e2e key' });
  await expect(row).toBeVisible();
  expect((await cdp.send('WebAuthn.getCredentials', { authenticatorId })).credentials.length).toBe(1);

  // signing out and in again: the password is no longer enough; the key finishes it
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await signIn();
  await expect(page.getByText('Use your passkey or security key to finish signing in.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeHidden();
  await page.getByRole('button', { name: 'Use passkey or security key' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  const creds = (await cdp.send('WebAuthn.getCredentials', { authenticatorId })).credentials;
  expect(creds[0].signCount).toBeGreaterThan(0); // the key counted the sign-in

  // with the key taken away from the browser, the second step cannot be completed
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await cdp.send('WebAuthn.clearCredentials', { authenticatorId });
  await signIn();
  await page.getByRole('button', { name: 'Use passkey or security key' }).click();
  await expect(page.getByText(/The key was not used|could not sign you in/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeHidden();

  // an administrator removes the second factor of a person who lost the key; then the password signs in again
  const users = await (await adm.request.fetch('/api/v1/users', { headers: H })).json();
  const me = users.find(u => u.email === email);
  expect((await adm.request.fetch(`/api/v1/users/${me.id}/2fa/reset`, { method: 'POST', headers: H })).status()).toBe(200);
  await signIn();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
});
