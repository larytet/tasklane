import { test, expect } from '@playwright/test';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 6).toUpperCase();

async function adminCtx(browser) {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  const call = async (method, path, data) => {
    const res = await ctx.request.fetch(path, { method, headers: H, data });
    let body = null; try { body = await res.json(); } catch (e) { /* none */ }
    return { status: res.status(), body };
  };
  return { ctx, page, call };
}

// UPG-10, PAUSE-04: during maintenance a person editing sees the banner and "Paused for maintenance, draft kept"; when it ends
// the banner goes and the save completes by itself, with nothing lost.
test('UPG-10: a save during maintenance keeps the draft, shows the banner and completes when it ends', async ({ browser }) => {
  const adm = await adminCtx(browser);
  const pk = 'P' + uniq();
  expect((await adm.call('POST', '/api/v1/projects', { key: pk, name: 'Pause test' })).status).toBe(201);
  const issue = (await adm.call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Pause target', description: 'before' })).body.key;
  await adm.page.goto(`/#/issue/${issue}`);
  await adm.page.getByRole('button', { name: 'Edit' }).click();
  const ta = adm.page.getByLabel('Description editor');
  await ta.fill('written during the maintenance window');
  const p = await adm.call('POST', '/api/v1/admin/pause', { scope: 'project:' + pk, owner: 'e2e', mode: 'pause', drain_wait_ms: 5000, hold_ms: 120000 });
  expect(p.status).toBe(200);
  await adm.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(adm.page.getByText('Paused for maintenance, draft kept')).toBeVisible({ timeout: 15000 });
  await expect(adm.page.getByText(/Maintenance in progress/)).toBeVisible();
  await expect(ta).toHaveValue('written during the maintenance window'); // the text is still in the editor
  // reads continue during the pause
  expect((await adm.call('GET', `/api/v1/issues/${issue}/view`)).status).toBe(200);
  expect((await adm.call('GET', '/api/v1/system/status')).body.maintenance.length).toBe(1);
  // the maintenance ends: the banner clears and the retried save lands
  expect((await adm.call('DELETE', `/api/v1/admin/pause/${p.body.id}`)).status).toBe(200);
  await expect(adm.page.getByText(/Maintenance in progress/)).toBeHidden({ timeout: 15000 });
  await expect(adm.page.getByText('written during the maintenance window')).toBeVisible({ timeout: 15000 });
  const got = await adm.call('GET', `/api/v1/issues/${issue}/view`);
  expect(JSON.stringify(got.body)).toContain('written during the maintenance window');
});

// UPG-08: a page the server no longer serves is told to reload; its draft is kept and comes back after the reload.
test('UPG-08: a client that is too old is told to reload and keeps its draft', async ({ browser }) => {
  const adm = await adminCtx(browser);
  const pk = 'S' + uniq();
  await adm.call('POST', '/api/v1/projects', { key: pk, name: 'Skew test' });
  const issue = (await adm.call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Skew target', description: 'before' })).body.key;
  await adm.page.goto(`/#/issue/${issue}`);
  await adm.page.getByRole('button', { name: 'Edit' }).click();
  await adm.page.getByLabel('Description editor').fill('words typed in an old tab');
  await adm.page.waitForTimeout(2500); // the local draft is written every 2 s
  await adm.page.route(`**/api/v1/issues/${issue}/text/description`, route => route.fulfill({ status: 426, contentType: 'application/json', body: JSON.stringify({ error: 'this page is out of date', code: 'client_too_old', detail: { min: 2, reload: true } }) }));
  await adm.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(adm.page.getByText('Reload to continue, draft kept')).toBeVisible();
  await expect(adm.page.getByText(/A newer version of Tasklane is available/)).toBeVisible();
  await adm.page.unroute(`**/api/v1/issues/${issue}/text/description`);
  await adm.page.getByRole('button', { name: 'Reload', exact: true }).click();
  await adm.page.goto(`/#/issue/${issue}`);
  await expect(adm.page.getByText(/Restored your unsaved draft/)).toBeVisible();
});

// DUR-05: a save the server's validation refuses (a text over the size limit) says why and keeps what the person typed, in the editor and in the
// draft; shortening the text lets the same editor save it.
test('DUR-05: a refused save keeps the text, says why, and the corrected text saves', async ({ browser }) => {
  const adm = await adminCtx(browser);
  const pk = 'V' + uniq();
  await adm.call('POST', '/api/v1/projects', { key: pk, name: 'Validation test' });
  const issue = (await adm.call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Validation target', description: 'before' })).body.key;
  await adm.page.goto(`/#/issue/${issue}`);
  await adm.page.getByRole('button', { name: 'Edit' }).click();
  const ta = adm.page.getByLabel('Description editor');
  const big = 'x'.repeat((1 << 20) + 10);
  await ta.fill(big);
  await adm.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(adm.page.getByText(/too large/i)).toBeVisible({ timeout: 15000 });
  await expect(ta).toHaveValue(big); // nothing was thrown away
  const kept = await adm.call('GET', `/api/v1/issues/${issue}/view`);
  expect(JSON.stringify(kept.body)).toContain('before'); // and the saved text is untouched
  await ta.fill('short enough now');
  await adm.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(adm.page.getByText('short enough now')).toBeVisible({ timeout: 15000 });
  expect(JSON.stringify((await adm.call('GET', `/api/v1/issues/${issue}/view`)).body)).toContain('short enough now');
});
