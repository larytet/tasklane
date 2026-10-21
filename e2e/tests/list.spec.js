import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, closeContexts, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const DEFAULTS = { columns: ['type', 'key', 'summary', 'priority', 'assignee', 'status', 'updated'], sort: { field: '', dir: 'asc' }, list_view: 'short', density: 'default' };
const put = (page, data) => page.evaluate(async d => (await fetch('/api/v1/me/ui-prefs', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify(d) })).status, data);
async function adminCtx(browser, opts = {}) {
  const ctx = await browser.newContext(opts);
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
  return { ctx, page };
}
// a fresh project with n issues, for paging and sorting without other tests' issues in the way
async function project(page, n, prefix = 'L') {
  const pk = prefix + Array.from({ length: 4 }, () => 'ABCDEFGHJKLMNPQRSTUVWXYZ'[Math.floor(Math.random() * 24)]).join('');
  await page.evaluate(async ({ pk, n }) => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    await fetch('/api/v1/projects', { method: 'POST', headers: H, body: JSON.stringify({ key: pk, name: 'List ' + pk }) });
    const pr = ['lowest', 'low', 'medium', 'high', 'highest'];
    for (let b = 0; b < n; b += 10) await Promise.all(Array.from({ length: Math.min(10, n - b) }, (_, i) => fetch('/api/v1/issues', { method: 'POST', headers: H, body: JSON.stringify({ project: pk, type: 'Task', summary: `item ${String(b + i).padStart(3, '0')}`, priority: pr[(b + i) % 5] }) })));
  }, { pk, n });
  return pk;
}

test('LIST-02, LIST-03: choose, order and hide columns (a custom field among them), sort from a header; key and summary stay', async ({ browser }) => {
  const { page, ctx } = await adminCtx(browser, { viewport: { width: 1400, height: 900 } });
  const fname = 'Effort ' + Date.now();
  const f = await page.evaluate(async name => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    const r = await (await fetch('/api/v1/fields', { method: 'POST', headers: H, body: JSON.stringify({ name, type: 'number' }) })).json();
    await fetch('/api/v1/fields/' + r.id + '/contexts', { method: 'POST', headers: H, body: JSON.stringify({ global: true }) });
    return r;
  }, fname);
  const pk = await project(page, 5);
  await page.evaluate(async ({ pk, key }) => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    await fetch('/api/v1/issues', { method: 'POST', headers: H, body: JSON.stringify({ project: pk, type: 'Task', summary: 'with effort', custom: { [key]: 8 } }) });
  }, { pk, key: f.key });
  await page.goto('/#/?syntax=tql&q=' + encodeURIComponent('project = ' + pk));
  await expect(page.getByRole('listitem')).toHaveCount(6);
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('tab', { name: 'List' }).click();
  const dlg = page.getByRole('region', { name: 'Settings' });
  await expect(dlg.getByLabel('Show Key')).toBeDisabled();
  await expect(dlg.getByLabel('Show Summary')).toBeDisabled();
  await dlg.getByLabel('Show Assignee').uncheck();
  await dlg.getByLabel('Show Reporter').check();
  await dlg.getByLabel('Show ' + fname).check();
  await expect(dlg.getByRole('status')).toHaveText('Saved');
  await dlg.getByRole('button', { name: 'Move ' + fname + ' earlier' }).click();
  await expect(dlg.getByRole('button', { name: 'Move ' + fname + ' earlier' })).toBeFocused();
  await expect(dlg.getByRole('status')).toHaveText('Saved');
  await new AxeBuilder({ page }).include('#settings-page').analyze().then(r => expect(r.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]));
  await page.getByRole('button', { name: 'Done' }).click();
  const row = page.getByRole('listitem').filter({ hasText: 'with effort' });
  await expect(row.locator('.c-cf')).toHaveText('8');
  await expect(row.locator('.c-reporter')).toHaveText('Administrator');
  await expect(row.locator('.c-assignee')).toHaveCount(0);
  // the custom column comes before the reporter now (it was moved earlier)
  const order = await page.locator('.listhead .colhead').allTextContents();
  expect(order.indexOf(fname)).toBeLessThan(order.indexOf('Reporter'));
  // sort from the header: ascending, descending, then the query's own order
  const sortBtn = page.locator('.listhead').getByRole('button', { name: /^Summary/ });
  await sortBtn.click();
  await expect(sortBtn).toHaveAccessibleName(/^Summary.*sorted ascending$/);
  await expect(page.getByRole('listitem').first()).toContainText('item 000');
  await sortBtn.click();
  await expect(sortBtn).toHaveAccessibleName(/^Summary.*sorted descending$/);
  await expect(page.getByRole('listitem').first()).toContainText('with effort');
  await sortBtn.click();
  await expect(sortBtn).toHaveAttribute('aria-pressed', 'false');
  // the choice is the person's, on the server; a trashed field drops out of the list
  await page.evaluate(async id => fetch('/api/v1/fields/' + id, { method: 'DELETE', credentials: 'same-origin', headers: { 'X-Requested-With': 'tasklane' } }), f.id);
  await page.reload();
  await expect(page.getByRole('listitem')).toHaveCount(6);
  await expect(page.locator('.c-cf')).toHaveCount(0);
  expect(await put(page, DEFAULTS)).toBe(200);
  await ctx.close();
});

test('LIST-05: a sticky pager on desktop, the page in the address through reload and Back, j onto the next page, the selection cleared', async ({ browser }) => {
  test.setTimeout(120_000);
  const { page, ctx } = await adminCtx(browser, { viewport: { width: 1280, height: 700 } });
  const pk = await project(page, 130, 'P');
  await page.goto('/#/?syntax=tql&q=' + encodeURIComponent('project = ' + pk + ' ORDER BY key ASC'));
  await expect(page.getByRole('listitem')).toHaveCount(100);
  const pager = page.getByRole('navigation', { name: 'Pages' });
  await expect(pager).toBeInViewport(); // sticky: visible while the list is long
  await page.mouse.wheel(0, 1500);
  await expect(pager).toBeInViewport();
  await page.getByLabel('Select ' + pk + '-1', { exact: true }).check();
  await pager.getByRole('button', { name: 'Next ›' }).click();
  await expect(page.getByText('130 issues · 101–130')).toBeVisible();
  await expect(page).toHaveURL(/page=2/);
  await expect(page.locator('[role=list] input.rowsel:checked')).toHaveCount(0); // a page change clears the selection
  await expect(page.getByRole('toolbar', { name: 'Selected issues' })).toBeHidden();
  await page.reload();
  await expect(page.getByText('130 issues · 101–130')).toBeVisible();
  // open one, come back: the same page
  await page.getByRole('listitem').first().click();
  await expect(page).toHaveURL(/#\/issue\//);
  await page.goBack();
  await expect(page.getByText('130 issues · 101–130')).toBeVisible();
  // the pager is reachable by keyboard
  await pager.getByRole('button', { name: '‹ Prev' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('130 issues · 1–100')).toBeVisible();
  // j past the last row goes on to the next page's first row
  await page.evaluate(() => document.activeElement.blur());
  for (let i = 0; i < 101; i++) await page.keyboard.press('j');
  await expect(page.getByText('130 issues · 101–130')).toBeVisible();
  await expect(page.locator('.row[aria-current=true]')).toHaveAttribute('data-key', pk + '-101');
  // every issue exactly once over the pages
  const seen = [];
  for (const n of [1, 2]) {
    await page.goto('/#/?syntax=tql&page=' + n + '&q=' + encodeURIComponent('project = ' + pk + ' ORDER BY key ASC'));
    await expect(page.getByText(n === 1 ? '130 issues · 1–100' : '130 issues · 101–130')).toBeVisible();
    seen.push(...await page.locator('[role=list] > .row[data-key]').evaluateAll(rs => rs.map(r => r.dataset.key)));
  }
  expect(new Set(seen).size).toBe(130);
  await ctx.close();
});

test('LIST-05 phone: infinite scroll instead of a pager', async ({ browser }) => {
  test.setTimeout(120_000);
  const { page, ctx } = await adminCtx(browser, { viewport: { width: 390, height: 800 } });
  const pk = await project(page, 120, 'M');
  await page.goto('/#/?syntax=tql&q=' + encodeURIComponent('project = ' + pk));
  await expect(page.getByRole('listitem')).toHaveCount(100);
  await expect(page.getByRole('navigation', { name: 'Pages' })).toBeHidden();
  await page.locator('.more-sentinel').scrollIntoViewIfNeeded();
  await expect(page.getByRole('listitem')).toHaveCount(120);
  await ctx.close();
});

test('LIST-06: short and long view with every density and a wider column choice, without sideways scroll on a phone', async ({ browser }) => {
  const { page, ctx } = await adminCtx(browser, { viewport: { width: 390, height: 800 } });
  const pk = await project(page, 3, 'V');
  for (const view of ['short', 'long']) for (const density of ['compact', 'default', 'comfortable']) for (const columns of [DEFAULTS.columns, ['key', 'summary', 'status', 'reporter', 'labels', 'created', 'comments']]) {
    expect(await put(page, { list_view: view, density, columns })).toBe(200);
    await page.goto('/#/?syntax=tql&q=' + encodeURIComponent('project = ' + pk));
    await page.reload();
    await expect(page.getByRole('listitem')).toHaveCount(3);
    const o = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(o, `${view}, ${density}, ${columns.length} columns`).toBeLessThanOrEqual(0);
  }
  expect(await put(page, DEFAULTS)).toBe(200);
  await ctx.close();
});
