import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readFileSync } from 'node:fs';
import { pageErrors, watch, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 6);

test('SEC-04: an administrator sets up a SAML provider from its metadata; the sign-in page offers it; a forged response is refused', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await page.goto('/#/admin/identity');
  const name = 'saml' + uniq();
  await page.getByRole('button', { name: 'Add a provider' }).click();
  const dlg = page.getByRole('dialog');
  await dlg.getByLabel('Protocol').selectOption('saml');
  await dlg.getByLabel('Name', { exact: true }).fill(name);
  await dlg.getByLabel('Button label').fill('Corp SAML');
  // nothing entered: the reason is given
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(dlg.getByRole('alert')).toContainText('entity id');
  // the identity provider's metadata fills in the rest
  await dlg.getByLabel('Identity provider metadata').fill(readFileSync('../internal/saml/testdata/idp_metadata.xml', 'utf8'));
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Save' }).click();
  const row = page.getByRole('listitem').filter({ hasText: 'Corp SAML' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('SAML');
  await expect(row).toContainText('https://idp.example/entity');
  // the addresses to give the identity provider are shown on the provider
  await row.getByRole('button', { name: 'Edit Corp SAML' }).click();
  await expect(page.getByRole('dialog')).toContainText(`/api/v1/auth/saml/${name}/metadata`);
  await expect(page.getByRole('dialog')).toContainText('/api/v1/auth/saml/acs');
  await expect(page.getByRole('dialog')).toContainText('until');
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();
  // the metadata document
  const md = await page.request.get(`/api/v1/auth/saml/${name}/metadata`);
  expect(md.status()).toBe(200);
  expect(md.headers()['content-type']).toContain('xml');
  const doc = await md.text();
  expect(doc).toContain(`/api/v1/auth/saml/${name}/metadata`);
  expect(doc).toContain('AssertionConsumerService');

  // a signed-out visitor is offered it, and the button sends the browser to the identity provider with a request and a relay state
  const out = await browser.newContext();
  const op = await out.newPage();
  await op.goto('/');
  const link = op.getByRole('link', { name: 'Sign in with Corp SAML' });
  await expect(link).toBeVisible();
  const start = await out.request.get(`/api/v1/auth/sso/${name}/start?redirect=/`, { maxRedirects: 0 });
  expect(start.status()).toBe(302);
  const to = new URL(start.headers()['location']);
  expect(to.origin + to.pathname).toBe('https://idp.example/sso');
  expect(to.searchParams.get('SAMLRequest')).toBeTruthy();
  const relay = to.searchParams.get('RelayState');
  expect(relay).toBeTruthy();
  // a forged response for that request is refused, and the person lands on the sign-in page, told nothing about why
  const forged = Buffer.from('<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" Version="2.0"/>').toString('base64');
  const acs = await out.request.post('/api/v1/auth/saml/acs', { form: { SAMLResponse: forged, RelayState: relay }, maxRedirects: 0 });
  expect(acs.status()).toBe(302);
  expect(acs.headers()['location']).toContain('sso=denied');
  expect((await out.request.get('/api/v1/me')).status()).toBe(401);
  // a relay state works once: the forged attempt used it up
  const again = await out.request.post('/api/v1/auth/saml/acs', { form: { SAMLResponse: forged, RelayState: relay }, maxRedirects: 0 });
  expect(again.headers()['location']).toContain('sso=denied');

  // delete it so later tests see a plain sign-in page
  await page.getByRole('button', { name: 'Delete Corp SAML' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page.getByRole('listitem').filter({ hasText: 'Corp SAML' })).toHaveCount(0);
  await ctx.close(); await out.close();
});
