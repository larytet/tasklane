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
  await page.getByRole('link', { name: 'My details' }).click();
  await page.getByLabel('Name').fill('Cora <b>C.</b>');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Saved.')).toBeVisible();
  await page.reload();
  await page.getByRole('link', { name: 'My details' }).click();
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

test('REQ: a customer raises a request through a form, staff reply (and see who will read it), the customer replies', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'R' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Requests' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true, welcome: 'Hello' } })).status()).toBe(200);
  expect((await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Something is broken', description: 'Tell us what went wrong', help_text: 'Include what you clicked.' } })).status()).toBe(201);

  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  const addr = `req.${uniq()}@customer.example`;
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  const { code } = await sentTo(addr);
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(page.getByText('Signed in as')).toContainText(addr);
  await expect(page.getByText('You have no requests yet')).toBeVisible();
  await page.getByRole('link', { name: 'New request' }).click();
  await expect(page.getByText('Include what you clicked.')).toBeVisible();
  const axe1 = await new AxeBuilder({ page }).analyze();
  expect(axe1.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  const summary = 'Cannot pay <i>twice</i> ' + uniq();
  await page.getByLabel('Summary').fill(summary);
  await page.getByLabel('Details').fill('The button does nothing.');
  await page.getByRole('button', { name: 'Send request' }).click();
  await expect(page.getByRole('heading', { level: 2 })).toContainText(summary); // shown as text, tags and all
  const key = (await page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];
  await expect(page.getByText('The button does nothing.')).toBeVisible();
  await expect(page.locator('#portal')).not.toContainText('null'); // an empty part of the page is nothing, not the word null

  // staff: the ticket says it came from the portal and who will read a reply
  const jana = await browser.newContext();
  jana.on('page', watch);
  const sp = await jana.newPage();
  await sp.goto('/');
  await sp.getByLabel('Email').fill('admin@example.com');
  await sp.getByLabel('Password').fill('adminpass1');
  await sp.getByRole('button', { name: 'Sign in' }).click();
  await sp.goto('/#/issue/' + key);
  await expect(sp.getByText('Raised through the portal by')).toContainText(addr);
  await expect(sp.locator('#reply-audience')).toContainText('The customer (' + addr + ') will see this reply');
  await sp.getByLabel('Reply', { exact: true }).fill('We are on it.');
  await sp.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(sp.getByText('We are on it.').first()).toBeVisible();
  await sp.getByLabel('Reply', { exact: true }).fill('INTERNAL ONLY: customer is a VIP');
  await sp.getByLabel('Internal').check();
  await expect(sp.locator('#reply-audience')).toContainText('Internal note');
  await sp.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(sp.getByText('INTERNAL ONLY').first()).toBeVisible();

  // the customer sees the reply, not the note, and answers
  await page.reload();
  await expect(page.getByText('We are on it.')).toBeVisible();
  await expect(page.getByText('INTERNAL ONLY')).toHaveCount(0);
  await expect(page.getByText('Support', { exact: true })).toBeVisible();
  await page.getByLabel('Add a reply').fill('Thanks, standing by');
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByText('Thanks, standing by')).toBeVisible();
  await expect(page.getByText('You', { exact: true })).toBeVisible();
  const axe2 = await new AxeBuilder({ page }).analyze();
  expect(axe2.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // the staff thread shows the customer's reply
  await sp.reload();
  await expect(sp.getByText('Thanks, standing by')).toBeVisible();
  // the portal settings page lists the form and passes the accessibility check
  await sp.goto('/#/portal-admin/' + pk);
  await expect(sp.getByRole('listitem', { name: '' }).filter({ hasText: 'Something is broken' })).toBeVisible();
  const axe3 = await new AxeBuilder({ page: sp }).analyze();
  expect(axe3.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // another customer cannot open it
  const other = await browser.newContext();
  const op = await other.newPage();
  await op.goto('/portal/' + pk);
  const addr2 = `other.${uniq()}@customer.example`;
  await op.getByLabel('Your email address').fill(addr2);
  await op.getByRole('button', { name: 'Email me a code' }).click();
  const c2 = await sentTo(addr2);
  await op.getByLabel('The six digit code we sent you').fill(c2.code);
  await op.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(op.getByText('Signed in as')).toBeVisible();
  const r = await other.request.get(`/api/v1/portal/${pk}/requests/${key}`);
  expect(r.status()).toBe(404);
  await op.goto(`/portal/${pk}#/r/${key}`);
  await expect(op.getByText('That request does not exist.')).toBeVisible();
});

test('PORT-08: a request carries service level clocks that staff see running, and a public reply meets the first response goal', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'S' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Service' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  const rt = await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Help', sla: { first_response_minutes: 60, resolution_minutes: 600, pause_statuses: ['In Progress'] } } });
  expect(rt.status()).toBe(201);
  // a policy the workflow cannot satisfy is refused with the reason
  const bad = await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Bad', sla: { first_response_minutes: 30, pause_statuses: ['Waiting for customer'] } } });
  expect(bad.status()).toBe(400);
  expect((await bad.json()).error).toContain('Waiting for customer');

  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  const addr = `sla.${uniq()}@customer.example`;
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  const { code } = await sentTo(addr);
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await page.getByRole('link', { name: 'New request' }).click();
  await page.getByLabel('Summary').fill('Nothing works');
  await page.getByRole('button', { name: 'Send request' }).click();
  await expect(page.getByRole('heading', { level: 2 })).toContainText('Nothing works');
  const key = (await page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];
  // the customer is not shown the clocks
  await expect(page.getByText('First response')).toHaveCount(0);

  const sp = await adm.newPage();
  await sp.goto('/#/issue/' + key);
  const clocks = sp.getByRole('group', { name: 'Service level goals' });
  await expect(clocks).toContainText('First response:');
  await expect(clocks).toContainText('left');
  await expect(clocks).toContainText('Resolution:');
  const axe = await new AxeBuilder({ page: sp }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // a note does not answer the customer; a public reply does
  await sp.getByLabel('Reply', { exact: true }).fill('private note');
  await sp.getByLabel('Internal').check();
  await sp.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(sp.getByText('private note').first()).toBeVisible();
  await expect(clocks).toContainText('First response: ');
  await expect(clocks).not.toContainText('First response: met');
  await sp.getByLabel('Reply', { exact: true }).fill('We are looking at it.');
  await sp.getByLabel('Internal').uncheck();
  await sp.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(sp.getByText('We are looking at it.').first()).toBeVisible();
  await expect(clocks).toContainText('First response: met');
  await expect(clocks).not.toContainText('First response: met late');
});
