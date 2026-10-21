import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, watch, closeContexts } from './helpers.js';

// Jira in the administrator panel and in a person's Settings (SPEC 18.1.1, WT-21, WT-22). There is no Jira in this run: the server's
// answers are those of a connection and a project, kept in a small state here by request interception, so what is tested is the page.

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const TOKEN_PAGE = 'https://id.atlassian.com/manage-profile/security/api-tokens';
const json = (route, body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

async function adminPage(browser) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  return { ctx, page };
}

test('WT-21: the Jira tab unlocks a connection and turns writing to Jira on for a project, after explaining it', async ({ browser }) => {
  const { page } = await adminPage(browser);
  const st = { locked: true, mode: 'read_only', tokens: 0, unlockBody: null, modes: [] };
  await page.route('**/api/v1/jira/connections', r => r.request().method() !== 'GET' ? r.fallback() : json(r, [{ id: 'c1', name: 'acme', site_url: 'https://acme.atlassian.net', email: 'sync@acme.example', token_fingerprint: 'ab12cd34', locked: st.locked, live_tokens: st.tokens, token_page: TOKEN_PAGE }]));
  await page.route('**/api/v1/jira/status', r => json(r, [{ project: 'WEB', jira_key: 'WEB', connection: 'acme', synced_at: new Date(Date.now() - 120000).toISOString(), issues: 12, mode: st.mode, locked: st.locked, last_error: '', warnings: [] }]));
  await page.route('**/api/v1/jira/connections/c1/unlock', r => { st.unlockBody = r.request().postDataJSON(); st.locked = false; return json(r, { ok: true }); });
  await page.route('**/api/v1/jira/projects/WEB/mode', r => { const b = r.request().postDataJSON(); st.modes.push(b.mode); st.mode = b.mode; return json(r, { mode: b.mode }); });

  await page.goto('/#/admin/jira');
  await expect(page.getByRole('heading', { name: 'Jira', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Jira', exact: true })).toHaveAttribute('aria-current', 'page'); // the tab
  await expect(page.getByText('Locked: waiting for the account’s token')).toBeVisible();
  await expect(page.getByText('sync@acme.example', { exact: false }).first()).toBeVisible();

  // unlock: the dialog says where the token stays and where it is created
  await page.getByRole('button', { name: 'Unlock acme' }).click();
  const dlg = page.getByRole('dialog', { name: 'Unlock acme' });
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText('kept only in memory');
  await expect(dlg).toContainText('never written to disk or to the database');
  await expect(dlg.getByRole('link', { name: 'Create an API token' })).toHaveAttribute('href', TOKEN_PAGE);
  await expect(dlg.getByText(TOKEN_PAGE)).toBeVisible(); // the address itself is shown too
  const tok = dlg.getByLabel('API token of sync@acme.example');
  await expect(tok).toHaveAttribute('type', 'password');
  const ax = await new AxeBuilder({ page }).include('[role=dialog]').analyze();
  expect(ax.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);
  await tok.fill('service-token-123');
  await dlg.getByRole('button', { name: 'Unlock' }).click();
  await expect(dlg).toBeHidden();
  expect(st.unlockBody).toEqual({ token: 'service-token-123' });
  await expect(page.getByText('Unlocked', { exact: true })).toBeVisible();

  // writing to Jira: explained first; Cancel leaves the project as it was
  const mode = page.getByLabel('What happens to changes made in Tasklane to WEB');
  await expect(mode).toHaveValue('read_only');
  await mode.selectOption('write_through');
  const sure = page.getByRole('dialog', { name: 'Save changes to Jira from WEB?' });
  await expect(sure).toBeVisible();
  await expect(sure).toContainText('as the person who made it');
  await expect(sure).toContainText('Jira decides who may edit');
  await expect(sure).toContainText('never overwritten without their say');
  await sure.getByRole('button', { name: 'Cancel' }).click();
  await expect(sure).toBeHidden();
  await expect(mode).toHaveValue('read_only');
  expect(st.modes).toEqual([]);
  await mode.selectOption('write_through');
  await page.getByRole('dialog', { name: 'Save changes to Jira from WEB?' }).getByRole('button', { name: 'Save changes to Jira' }).click();
  await expect.poll(() => st.modes).toEqual(['write_through']);
  await expect(page.getByLabel('What happens to changes made in Tasklane to WEB')).toHaveValue('write_through');
  // and back: no question, and it says the tokens were forgotten
  await page.getByLabel('What happens to changes made in Tasklane to WEB').selectOption('read_only');
  await expect.poll(() => st.modes).toEqual(['write_through', 'read_only']);
  await expect(page.locator('.toast').filter({ hasText: 'tokens for it were forgotten' })).toBeVisible();
  const page_ax = await new AxeBuilder({ page }).analyze();
  expect(page_ax.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);
});

test('WT-21: a token Jira refuses at unlock is explained in the dialog and the connection stays locked', async ({ browser }) => {
  const { page } = await adminPage(browser);
  await page.route('**/api/v1/jira/connections', r => r.request().method() !== 'GET' ? r.fallback() : json(r, [{ id: 'c1', name: 'acme', site_url: 'https://acme.atlassian.net', email: 'sync@acme.example', locked: true, live_tokens: 0, token_page: TOKEN_PAGE }]));
  await page.route('**/api/v1/jira/status', r => json(r, []));
  await page.route('**/api/v1/jira/connections/c1/unlock', r => json(r, { error: 'Jira rejected the credentials: check the API token', code: 'invalid' }, 400));
  await page.goto('/#/admin/jira');
  await page.getByRole('button', { name: 'Unlock acme' }).click();
  const dlg = page.getByRole('dialog', { name: 'Unlock acme' });
  await dlg.getByLabel('API token of sync@acme.example').fill('wrong');
  await dlg.getByRole('button', { name: 'Unlock' }).click();
  await expect(dlg.getByRole('alert')).toContainText('Jira rejected the credentials');
  await expect(dlg.getByRole('button', { name: 'Unlock' })).toBeEnabled(); // it can be tried again
  await dlg.getByRole('button', { name: 'Cancel' }).click();
  await expect(page.getByText('Locked: waiting for the account’s token')).toBeVisible();
});

test('WT-21: no Jira connection says how to make one', async ({ browser }) => {
  const { page } = await adminPage(browser);
  await page.route('**/api/v1/jira/connections', r => r.request().method() !== 'GET' ? r.fallback() : json(r, []));
  await page.route('**/api/v1/jira/status', r => json(r, []));
  await page.goto('/#/admin/jira');
  await expect(page.getByText('No Jira connection.')).toBeVisible();
  await expect(page.getByText('No project is mirrored from Jira.')).toBeVisible();
});

test('WT-22: a person sets, replaces and forgets their Jira token in Settings, with a link to where it is created', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const st = { has: false, body: null, forgot: 0 };
  const info = () => [{ connection_id: 'c1', connection: 'acme', site_url: 'https://acme.atlassian.net', token_page: TOKEN_PAGE, has_token: st.has, projects: ['WEB', 'OPS'] }];
  await page.route('**/api/v1/jira/session-token**', r => {
    const m = r.request().method();
    if (m === 'GET') return json(r, info());
    if (m === 'POST') { st.body = r.request().postDataJSON(); st.has = true; return json(r, { ok: true }); }
    if (m === 'DELETE') { st.has = false; st.forgot++; return json(r, { ok: true }); }
    return r.fallback();
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  const tab = page.getByRole('tab', { name: 'Jira' });
  await expect(tab).toBeVisible();
  await tab.click();
  const panel = page.getByRole('tabpanel', { name: 'Jira' });
  await expect(panel).toContainText('kept only in memory for this session');
  await expect(panel).toContainText('never written to disk or to the database');
  await expect(panel).toContainText('WEB, OPS');
  await expect(panel.getByText('No token: you are asked at your first save')).toBeVisible();
  await expect(panel.getByRole('link', { name: 'Where to create a token for acme' })).toHaveAttribute('href', TOKEN_PAGE);
  const ax = await new AxeBuilder({ page }).include('[role=dialog]').analyze();
  expect(ax.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);

  await panel.getByRole('button', { name: 'Set your token for acme' }).click();
  const dlg = page.getByRole('dialog', { name: 'Save to Jira as you' });
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText('Jira: acme (https://acme.atlassian.net)');
  await expect(dlg.getByRole('link', { name: 'Create an API token' })).toHaveAttribute('href', TOKEN_PAGE);
  await dlg.getByLabel('Jira API token').fill('my-token-1');
  await dlg.getByRole('button', { name: 'Save token' }).click();
  await expect(dlg).toBeHidden();
  expect(st.body).toEqual({ connection: 'c1', email: '', token: 'my-token-1' });
  await expect(panel.getByText('Token set for this session')).toBeVisible();
  await expect(panel.getByRole('button', { name: 'Replace your token for acme' })).toBeVisible();
  // the token is never shown again
  await expect(page.locator('body')).not.toContainText('my-token-1');

  await panel.getByRole('button', { name: 'Forget your token for acme' }).click();
  await expect(panel.getByText('No token: you are asked at your first save')).toBeVisible();
  expect(st.forgot).toBe(1);
});

test('WT-22: with no project that saves to Jira, Settings has no Jira tab', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('tab', { name: 'Appearance' })).toBeVisible();
  await expect(page.getByRole('tab', { name: 'Jira' })).toHaveCount(0);
  await expect(page.getByRole('tab')).toHaveCount(6); // Appearance, List, Keyboard, Security, Sessions, API tokens
});
