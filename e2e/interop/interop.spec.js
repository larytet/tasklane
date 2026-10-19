import { test, expect } from '@playwright/test';

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };

function need(name) {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set: run this under \`make interop\` (it does not skip)`);
  return v;
}

async function signIn(browser, base, email, password) {
  const ctx = await browser.newContext({ baseURL: base });
  const pageErrors = [];
  const page = await ctx.newPage();
  page.on('pageerror', e => pageErrors.push(String(e)));
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  return { ctx, page, pageErrors };
}

test('INTEROP-06: two people on different nodes see each other, and the search box is answered by OpenSearch', async ({ browser }) => {
  const app1 = need('INTEROP_APP1'), app2 = need('INTEROP_APP2');
  const a = await signIn(browser, app1, 'admin@example.com', 'adminpass1');
  const call = async (method, path, data) => { const r = await a.ctx.request.fetch(app1 + path, { method, headers: H, data }); return { status: r.status(), body: await r.json().catch(() => null) }; };
  const pk = 'PW' + Math.random().toString(36).slice(2, 5).toUpperCase();
  expect((await call('POST', '/api/v1/projects', { key: pk, name: 'Playwright interop' })).status).toBe(201);
  const u = await call('POST', '/api/v1/users', { email: `pia.${pk.toLowerCase()}@example.com`, name: 'Pia Interop', password: 'piapassword1' });
  expect(u.status).toBe(201);
  expect((await call('POST', `/api/v1/projects/${pk}/members`, { user: u.body.id, role: 'member' })).status).toBe(200);
  const made = await call('POST', '/api/v1/issues', { project: pk, type: 'Task', summary: 'Interoperable gizmo', description: 'findable by the search box' });
  expect(made.status).toBe(201);
  const key = made.body.key;

  const b = await signIn(browser, app2, `pia.${pk.toLowerCase()}@example.com`, 'piapassword1');
  await a.page.goto(`/#/issue/${key}`);
  await b.page.goto(`/#/issue/${key}`);
  await expect(a.page.getByText(key).first()).toBeVisible();
  // the presence of each person reaches the other node's page (through Redis)
  await expect(a.page.getByText('Pia Interop is here')).toBeVisible({ timeout: 30_000 });
  await expect(b.page.getByLabel('1 others here')).toBeVisible({ timeout: 30_000 });

  // the search box on node 2: the response says OpenSearch answered
  await b.page.goto('/#/search');
  const box = b.page.getByLabel('Search or TQL');
  await expect(box).toBeVisible();
  const answered = b.page.waitForResponse(async r => {
    if (!r.url().includes('/api/v1/issues?') || r.request().method() !== 'GET') return false;
    if (!decodeURIComponent(r.url()).includes('gizmo')) return false;
    const j = await r.json().catch(() => null);
    return !!j && j.engine === 'opensearch' && (j.issues || []).some(i => i.key === key);
  }, { timeout: 60_000 });
  // the index may be a few seconds behind the write: ask again until it has caught up
  for (let i = 0; i < 20; i++) {
    await box.fill('');
    await box.fill('text ~ "gizmo"');
    await box.press('Enter');
    const got = await Promise.race([answered.then(() => true), new Promise(r => setTimeout(() => r(false), 4000))]);
    if (got) break;
  }
  await answered;
  await expect(b.page.getByText(key).first()).toBeVisible();
  expect(a.pageErrors).toEqual([]);
  expect(b.pageErrors).toEqual([]);
});
