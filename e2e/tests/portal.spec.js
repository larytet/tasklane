import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createHmac } from 'node:crypto';
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
  await sp.getByRole('tab', { name: /^Comments/ }).click();
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
  await sp.getByRole('tab', { name: /^Comments/ }).click();
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
  await sp.getByRole('tab', { name: /^Comments/ }).click();
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

test('PORT-03: a customer shares a request with their company, a colleague joins the conversation, and taking it back is immediate', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'C' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Companies' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  expect((await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Question' } })).status()).toBe(201);
  const dom = `${uniq()}.example`;
  const cora = `cora@${dom}`, dave = `dave@${dom}`;
  const org = await adm.request.post(`/api/v1/projects/${pk}/portal/customer-orgs`, { headers: H, data: { name: 'Acme ' + uniq() } });
  expect(org.status()).toBe(201);
  const oid = (await org.json()).id;
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal/customer-orgs/${oid}/members`, { headers: H, data: { add: [cora, dave] } })).status()).toBe(200);

  async function signIn(addr) {
    const ctx = await browser.newContext();
    ctx.on('page', watch);
    const page = await ctx.newPage();
    await page.goto('/portal/' + pk);
    await page.getByLabel('Your email address').fill(addr);
    await page.getByRole('button', { name: 'Email me a code' }).click();
    const { code } = await sentTo(addr);
    await page.getByLabel('The six digit code we sent you').fill(code);
    await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
    await expect(page.getByText('Signed in as')).toContainText(addr);
    return { ctx, page };
  }
  const c = await signIn(cora);
  await c.page.getByRole('link', { name: 'New request' }).click();
  await c.page.getByLabel('Summary').fill('Our invoice is wrong');
  await c.page.getByRole('button', { name: 'Send request' }).click();
  await expect(c.page.getByRole('heading', { level: 2 })).toContainText('Our invoice is wrong');
  const key = (await c.page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];
  const sharing = c.page.getByRole('region', { name: 'Sharing' });
  await expect(sharing).toContainText('Only you, unless you share it');
  const axe = await new AxeBuilder({ page: c.page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);

  // a colleague sees nothing yet
  const d = await signIn(dave);
  await expect(d.page.getByText('You have no requests yet')).toBeVisible();
  expect((await d.ctx.request.get(`/api/v1/portal/${pk}/requests/${key}`)).status()).toBe(404);
  // shared with the company: the colleague sees it, by the owner's name, and joins in
  await sharing.getByLabel(/Everyone at Acme/).check();
  await expect(c.page.getByRole('region', { name: 'Sharing' }).getByLabel(/Everyone at Acme/)).toBeChecked();
  await d.page.reload();
  await expect(d.page.getByRole('link', { name: key + ' Our invoice is wrong' })).toBeVisible();
  await d.page.getByRole('link', { name: key + ' Our invoice is wrong' }).click();
  await expect(d.page.getByText('raised by ' + cora)).toBeVisible();
  await expect(d.page.getByRole('region', { name: 'Sharing' })).toHaveCount(0); // only the owner changes it
  await d.page.getByLabel('Add a reply').fill('We saw the same');
  await d.page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(d.page.getByText('We saw the same')).toBeVisible();
  await c.page.reload();
  await expect(c.page.getByRole('list', { name: 'Conversation' })).toContainText(dave);
  // taking it back
  await c.page.getByRole('region', { name: 'Sharing' }).getByLabel(/Everyone at Acme/).uncheck();
  await expect(c.page.getByRole('region', { name: 'Sharing' }).getByLabel(/Everyone at Acme/)).not.toBeChecked();
  expect((await d.ctx.request.get(`/api/v1/portal/${pk}/requests/${key}`)).status()).toBe(404);
});

// the newest non-code message to an address in the sink, decoded
async function mailTo(addr, includes) {
  for (let i = 0; i < 60; i++) {
    const dir = process.env.E2E_MAIL_DIR;
    for (const f of readdirSync(dir).sort().reverse()) {
      const raw = readFileSync(join(dir, f), 'utf8').replace(/=\r?\n/g, '').replace(/=([0-9A-F]{2})/g, (m, h) => String.fromCharCode(parseInt(h, 16)));
      if (raw.toLowerCase().includes('<' + addr.toLowerCase() + '>') && !raw.includes('sign-in code') && raw.includes(includes)) return raw;
    }
    await new Promise(r => setTimeout(r, 250));
  }
  throw new Error('no mail to ' + addr + ' with ' + includes);
}

test('PORT-07: the customer is emailed about their request with a portal link, answers by email, and the answer is their comment', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'M' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Mail desk' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  expect((await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Question' } })).status()).toBe(201);
  const src = await adm.request.post('/api/v1/admin/mail/sources', { headers: H, data: { name: 'e2e ' + uniq(), trusted: true } });
  expect(src.status()).toBe(201);
  const source = await src.json();

  const dom = `${uniq()}.example`, addr = `cora@${dom}`;
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  const { code } = await sentTo(addr);
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await page.getByRole('link', { name: 'New request' }).click();
  await page.getByLabel('Summary').fill('Mail me about this');
  await page.getByRole('button', { name: 'Send request' }).click();
  await expect(page.getByRole('heading', { level: 2 })).toContainText('Mail me about this');
  const key = (await page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];

  // the confirmation: the key in the subject, a link to the portal, a reply address, and nothing about staff
  const conf = await mailTo(addr, 'We received your request');
  expect(conf).toContain(`[${key}]`);
  expect(conf).toContain(`/portal/${pk}#/r/${key}`);
  const replyTo = conf.match(/^Reply-To: (.+)$/mi)[1].trim();
  expect(replyTo).toMatch(/^reply\+/);

  // staff reply publicly and privately: only the public one is mailed, without the name
  const sp = await adm.newPage();
  await sp.goto('/#/issue/' + key);
  await sp.getByRole('tab', { name: /^Comments/ }).click();
  await sp.getByLabel('Reply', { exact: true }).fill('Please send a photo.');
  await sp.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(sp.getByText('Please send a photo.').first()).toBeVisible();
  const said = await mailTo(addr, 'Please send a photo.');
  expect(said).toContain('Support replied to your request');
  expect(said).not.toContain('Administrator');

  // the customer answers by email (the relay is trusted and says the message is authenticated)
  const raw = `From: ${addr}\r\nTo: ${replyTo}\r\nSubject: Re: [${key}] Mail me about this\r\nMessage-ID: <${uniq()}@${dom}>\r\nAuthentication-Results: mx.example.net; dkim=pass header.d=${dom}\r\nContent-Type: text/plain\r\n\r\nHere is the photo link.\r\n`;
  const sig = 'sha256=' + createHmac('sha256', source.secret).update(raw).digest('hex');
  const res = await adm.request.fetch(`/api/v1/mail/inbound/${source.id}`, { method: 'POST', headers: { 'X-Tasklane-Signature': sig }, data: raw });
  expect(res.status()).toBe(200);
  expect((await res.json()).outcome).toBe('accepted');
  await page.reload();
  await expect(page.getByText('Here is the photo link.')).toBeVisible();
  await sp.reload();
  await sp.getByRole('tab', { name: /^Comments/ }).click();
  await expect(sp.getByText('Here is the photo link.')).toBeVisible();
});

test('PORT-06: a customer downloads their data and erases themselves; an administrator exports, promotes and erases customers', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'D' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Data' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  expect((await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Question' } })).status()).toBe(201);

  async function signIn(addr) {
    const ctx = await browser.newContext();
    ctx.on('page', watch);
    const page = await ctx.newPage();
    await page.goto('/portal/' + pk);
    await page.getByLabel('Your email address').fill(addr);
    await page.getByRole('button', { name: 'Email me a code' }).click();
    const { code } = await sentTo(addr);
    await page.getByLabel('The six digit code we sent you').fill(code);
    await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
    await expect(page.getByText('Signed in as')).toContainText(addr);
    return { ctx, page };
  }
  const gone = `gone.${uniq()}@customer.example`, keep = `keep.${uniq()}@customer.example`;
  const g = await signIn(gone);
  await g.page.getByRole('link', { name: 'New request' }).click();
  await g.page.getByLabel('Summary').fill('Please forget me ' + uniq());
  await g.page.getByRole('button', { name: 'Send request' }).click();
  await expect(g.page.getByRole('heading', { level: 2 })).toContainText('Please forget me');
  const key = (await g.page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];
  const k = await signIn(keep);

  // the customer downloads their data
  await g.page.getByRole('link', { name: 'My details' }).click();
  const [download] = await Promise.all([g.page.waitForEvent('download'), g.page.getByRole('button', { name: 'Download my data' }).click()]);
  const data = JSON.parse(readFileSync(await download.path(), 'utf8'));
  expect(data.profile.email).toBe(gone);
  expect(data.requests.length).toBe(1);
  expect(JSON.stringify(data)).not.toContain(keep);
  const axeD = await new AxeBuilder({ page: g.page }).analyze();
  expect(axeD.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);

  // an administrator finds a customer, exports their data and makes them staff
  const sp = await adm.newPage();
  await sp.goto('/#/portal-admin/' + pk);
  await sp.getByLabel('Find a customer').fill(keep);
  await expect(sp.getByRole('list', { name: 'Customers' })).toContainText(keep);
  const axeA = await new AxeBuilder({ page: sp }).analyze();
  expect(axeA.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  const [dl2] = await Promise.all([sp.waitForEvent('download'), sp.getByRole('button', { name: 'Export data of ' + keep }).click()]);
  expect(JSON.parse(readFileSync(await dl2.path(), 'utf8')).profile.email).toBe(keep);
  await sp.getByRole('button', { name: 'Promote ' + keep + ' to staff' }).click();
  await sp.getByLabel('Initial password').fill('a long enough one');
  await sp.getByRole('button', { name: 'Make staff', exact: true }).click();
  await expect(sp.getByText('Staff account made.')).toBeVisible();
  await expect(sp.getByRole('list', { name: 'Customers' })).toContainText('staff');

  // the customer erases themselves: they must type their address, and are signed out
  await g.page.getByText('Delete my data', { exact: true }).first().click();
  await g.page.getByRole('button', { name: 'Delete my data' }).click();
  await expect(g.page.getByRole('alert').or(g.page.getByRole('status')).filter({ hasText: 'type your email' })).toBeVisible();
  await g.page.getByLabel('Type your email address to confirm').fill(gone);
  await g.page.getByRole('button', { name: 'Delete my data' }).click();
  await expect(g.page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  expect((await g.ctx.request.get('/api/v1/portal/me')).status()).toBe(401);
  // staff: the request stays, with no trace of the customer
  const view = await adm.request.get(`/api/v1/issues/${key}/view`);
  expect(view.status()).toBe(200);
  const body = await view.text();
  expect(body).not.toContain(gone);
  expect(body).toContain('Deleted customer');
  await sp.getByLabel('Find a customer').fill('erased');
  await expect(sp.getByRole('list', { name: 'Customers' })).toContainText('erased');
  void k;
});

test('PORT-05: files on a request: off until the portal allows them, within its rules, shared both ways, removable', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'F' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Files desk' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  expect((await adm.request.post(`/api/v1/projects/${pk}/request-types`, { headers: H, data: { name: 'Help' } })).status()).toBe(201);
  const addr = `fay.${uniq()}@customer.example`;
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  const { code } = await sentTo(addr);
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(page.getByText('Signed in as')).toContainText(addr);
  await page.getByRole('link', { name: 'New request' }).click();
  await page.getByLabel('Summary').fill('My screen is cracked');
  await page.getByRole('button', { name: 'Send request' }).click();
  await expect(page.getByRole('heading', { level: 2 })).toContainText('My screen is cracked');
  const key = (await page.getByRole('heading', { level: 2 }).textContent()).split(' ')[0];
  // off by default: no upload control
  await expect(page.getByLabel('Add a file')).toHaveCount(0);
  expect((await ctx.request.post(`/api/v1/portal/${pk}/requests/${key}/attachments?name=a.txt`, { headers: { 'X-Requested-With': 'tasklane', 'Content-Type': 'text/plain' }, data: 'hello' })).status()).toBe(400);
  // the administrator allows files
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true, attach_enabled: true, attach_max_mb: 1, attach_max_files: 2 } })).status()).toBe(200);
  await page.reload();
  await expect(page.getByLabel('Add a file')).toBeVisible();
  await expect(page.getByText('Up to 1 MB each, 2 files per request')).toBeVisible();
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // a flagged file is refused and says so; an allowed one appears
  await page.getByLabel('Add a file').setInputFiles({ name: 'virus.txt', mimeType: 'text/plain', buffer: Buffer.from('X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*') });
  await page.getByRole('button', { name: 'Upload' }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'refused' })).toBeVisible();
  await page.getByLabel('Add a file').setInputFiles({ name: 'crack.txt', mimeType: 'text/plain', buffer: Buffer.from('a photo would go here') });
  await page.getByRole('button', { name: 'Upload' }).click();
  await expect(page.getByRole('link', { name: 'crack.txt' })).toBeVisible();
  await expect(page.getByText(/1 KB · yours/)).toBeVisible();
  // downloading gives the bytes, as a download
  const dl = await ctx.request.get(`/api/v1/portal/${pk}/attachments/` + (await page.getByRole('link', { name: 'crack.txt' }).getAttribute('href')).split('/').pop());
  expect(dl.status()).toBe(200);
  expect(dl.headers()['content-disposition']).toContain('attachment');
  expect(await dl.text()).toBe('a photo would go here');
  // staff see it as the customer's, and share a file of their own
  const issue = await (await adm.request.get(`/api/v1/issues/${key}/view`)).json();
  expect(issue.attachments.map(a => [a.name, a.from_customer, a.customer_visible])).toEqual([['crack.txt', true, true]]);
  const up = await adm.request.post(`/api/v1/issues/${key}/attachments?name=guide.txt`, { headers: { 'X-Requested-With': 'tasklane', 'Content-Type': 'text/plain' }, data: 'how to fix it' });
  expect(up.status()).toBe(201);
  const gid = (await up.json()).id;
  await page.reload();
  await expect(page.getByRole('link', { name: 'guide.txt' })).toHaveCount(0);
  expect((await adm.request.put(`/api/v1/attachments/${gid}/customer-visible`, { headers: H, data: { visible: true } })).status()).toBe(200);
  await page.reload();
  await expect(page.getByRole('link', { name: 'guide.txt' })).toBeVisible();
  // she can remove her own file but not the staff's
  await expect(page.getByRole('button', { name: 'Remove guide.txt' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Remove crack.txt' }).click();
  await expect(page.getByRole('link', { name: 'crack.txt' })).toHaveCount(0);
  expect((await ctx.request.delete(`/api/v1/portal/${pk}/attachments/${gid}`, { headers: H })).status()).toBe(403);
});

test('PORT-05: the sign-in check: a code is mailed only after the browser does the work; a direct call without it is refused', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  expect((await adm.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'K' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Checked desk' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true, captcha_bits: 12 } })).status()).toBe(200);
  const addr = `kim.${uniq()}@customer.example`;
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  // a script that skips the check gets nothing
  const raw = await ctx.request.post(`/api/v1/portal/${pk}/code`, { headers: H, data: { email: addr } });
  expect(raw.status()).toBe(400);
  expect((await raw.json()).error).toContain('check');
  const page = await ctx.newPage();
  await page.goto('/portal/' + pk);
  await page.getByLabel('Your email address').fill(addr);
  await page.getByRole('button', { name: 'Email me a code' }).click();
  await expect(page.getByText('a code is on its way')).toBeVisible();
  const { code } = await sentTo(addr);
  await page.getByLabel('The six digit code we sent you').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).first().click();
  await expect(page.getByText('Signed in as')).toContainText(addr);
});

test('PORT-01 (identity provider): a portal offers sign-in through its customers\' identity provider; it needs address domains; a forged response is refused and lands back on the portal', async ({ browser }) => {
  const adm = await browser.newContext();
  adm.on('page', watch);
  const ap = await adm.newPage();
  await ap.goto('/');
  await ap.getByLabel('Email').fill('admin@example.com');
  await ap.getByLabel('Password').fill('adminpass1');
  await ap.getByRole('button', { name: 'Sign in' }).click();
  await expect(ap.getByRole('button', { name: 'Create new' })).toBeVisible();
  const pk = 'S' + uniq().toUpperCase();
  expect((await adm.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Sso desk' } })).status()).toBe(201);
  expect((await adm.request.put(`/api/v1/projects/${pk}/portal`, { headers: H, data: { enabled: true } })).status()).toBe(200);
  await ap.goto(`/#/portal-admin/${pk}`);
  const box = ap.locator('#pe-sso');
  await expect(box).toBeVisible();
  const axe = await new AxeBuilder({ page: ap }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await box.getByLabel('Protocol').selectOption('saml');
  await box.getByLabel('Button label').fill('Acme login');
  await box.getByLabel('Identity provider metadata').fill(readFileSync('../internal/saml/testdata/idp_metadata.xml', 'utf8'));
  await box.getByRole('button', { name: 'Save sign-in provider' }).click();
  await expect(box.getByRole('alert')).toContainText('domains');
  await box.getByLabel('Address domains', { exact: true }).fill('example.com');
  await box.getByRole('button', { name: 'Save sign-in provider' }).click();
  await expect(box).toContainText('/api/v1/auth/saml/portal-' + pk.toLowerCase() + '/metadata');
  // the staff sign-in page does not offer it
  const out = await browser.newContext();
  const sp = await out.newPage();
  await sp.goto('/');
  await expect(sp.getByRole('link', { name: 'Sign in with Acme login' })).toHaveCount(0);
  // the portal does
  const page = await out.newPage();
  await page.goto('/portal/' + pk);
  const link = page.getByRole('link', { name: 'Sign in with Acme login' });
  await expect(link).toBeVisible();
  const start = await out.request.get(`/api/v1/auth/sso/portal-${pk.toLowerCase()}/start?redirect=/portal/${pk}`, { maxRedirects: 0 });
  expect(start.status()).toBe(302);
  const to = new URL(start.headers()['location']);
  expect(to.origin + to.pathname).toBe('https://idp.example/sso');
  const relay = to.searchParams.get('RelayState');
  // a response that is not signed by the provider is refused, and the person comes back to the portal
  const forged = Buffer.from('<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>').toString('base64');
  const post = await out.request.post('/api/v1/auth/saml/acs', { form: { SAMLResponse: forged, RelayState: relay }, maxRedirects: 0 });
  expect(post.status()).toBe(302);
  expect(post.headers()['location']).toBe(`/portal/${pk}?sso=denied`);
  expect((await out.request.get('/api/v1/portal/me')).status()).toBe(401);
  await page.goto(`/portal/${pk}?sso=denied`);
  await expect(page.getByRole('alert')).toContainText('not accepted');
  // removing it takes the button away
  await ap.getByRole('button', { name: 'Remove it' }).click();
  await page.goto('/portal/' + pk);
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Sign in with Acme login' })).toHaveCount(0);
});
