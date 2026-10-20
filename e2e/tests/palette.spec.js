import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, watch, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];
async function axe(page, what) {
  const r = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const bad = r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
  expect(bad.map(v => v.id + ': ' + v.nodes.slice(0, 2).map(n => n.target.join(' ')).join(', ')), what).toEqual([]);
}
const call = (page, method, path, body) => page.evaluate(async ({ method, path, body }) => {
  const r = await fetch(path, { method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: body ? JSON.stringify(body) : undefined });
  return { status: r.status, data: await r.json().catch(() => null) };
}, { method, path, body });
async function newAdmin(browser) { // the administrator has their own password, not the people's shared test one
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
const box = page => page.getByRole('combobox', { name: 'Go to or do' });
const options = page => page.getByRole('listbox', { name: 'Results' }).getByRole('option');
async function open(page) {
  await page.keyboard.press('Control+k');
  await expect(box(page)).toBeFocused();
}
async function find(page, text) {
  await box(page).fill(text);
  await expect(options(page).first()).toBeVisible();
}

test('PAL-01: Ctrl+K and Cmd+K open the palette from any screen, a text field included', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'PAL-01 issue', 'x');
  for (const [where, chord] of [['/', 'Control+k'], ['/#/issue/' + key, 'Meta+k'], ['/#/dashboards', 'Control+k']]) {
    await page.goto(where);
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    await page.keyboard.press(chord);
    await expect(box(page), where + ' ' + chord).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(box(page)).toHaveCount(0);
  }
  await page.goto('/#/issue/' + key);
  await page.getByRole('tab', { name: /^Comments/ }).click();
  await page.getByPlaceholder('Reply…').fill('half a thought');
  await page.getByPlaceholder('Reply…').press('Control+k');
  await expect(box(page)).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByPlaceholder('Reply…')).toHaveValue('half a thought'); // nothing typed into the field, focus back in it
  await expect(page.getByPlaceholder('Reply…')).toBeFocused();
  await ctx.close();
});

test('PAL-02: jump to an issue by key, by an old key after a move, by text; to a project, a board and a page', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'PAL-02 jump target', 'x');
  await page.goto('/');
  await open(page);
  await find(page, key.toLowerCase());
  await expect(options(page).first()).toContainText(key + ' PAL-02 jump target');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(new RegExp('#/issue/' + key + '$'));
  // moved to XYZ: the old key opens the current issue
  const moved = await call(page, 'POST', '/api/v1/issues/' + key + '/lifecycle', { op: 'move', project: 'XYZ' });
  expect(moved.status).toBe(200);
  const now = (await call(page, 'GET', '/api/v1/issues/' + key + '/view')).data.issue.key;
  expect(now).toMatch(/^XYZ-/);
  await open(page);
  await find(page, key);
  await expect(options(page).first()).toContainText(now + ' PAL-02 jump target (was ' + key + ')');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(new RegExp('#/issue/' + now + '$'));
  // a project, a board and a page
  const admin = await newAdmin(browser); // boards are made by project administrators
  const b = await call(admin.page, 'POST', '/api/v1/projects/XYZ/boards', { name: 'Pal board ' + Date.now(), kind: 'kanban' });
  expect(b.status).toBe(201);
  await admin.ctx.close();
  await open(page);
  await find(page, 'Xray');
  await expect(options(page).first()).toContainText('XYZ · Xray');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/project%20%3D%20XYZ/);
  await open(page);
  await find(page, b.data.name);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(new RegExp('#/board/' + b.data.id));
  await open(page);
  await find(page, 'releases');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/#\/releases(\/[A-Z]+)?$/); // the page opens the first project's releases
  await ctx.close();
});

test('PAL-03: actions run, and the ones a person may not take are absent', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  await open(jana.page);
  await box(jana.page).fill('new project');
  await jana.page.waitForTimeout(400);
  await expect(jana.page.getByRole('listbox', { name: 'Results' })).not.toContainText('New project');
  await box(jana.page).fill('admin');
  await expect(jana.page.getByRole('listbox', { name: 'Results' })).toContainText('Nothing matches');
  await find(jana.page, 'settings');
  await jana.page.keyboard.press('Enter');
  await expect(jana.page.getByRole('dialog', { name: 'Settings' })).toBeVisible();
  await jana.page.keyboard.press('Escape');
  await open(jana.page);
  await find(jana.page, 'create issue');
  await jana.page.keyboard.press('Enter');
  await expect(jana.page.getByRole('dialog')).toContainText('Create');
  await jana.page.keyboard.press('Escape');
  // a shortcut action on the issue page: Assign
  const key = await createIssue(jana.page, 'PAL-03 assign', 'x');
  await jana.page.goto('/#/issue/' + key);
  await expect(jana.page.getByLabel('Summary')).toBeVisible();
  await open(jana.page);
  await find(jana.page, 'assign');
  await expect(options(jana.page).filter({ hasText: 'Assign' })).toHaveCount(1);
  await jana.page.keyboard.press('Enter');
  await expect(jana.page.getByRole('dialog', { name: 'Assign ' + key })).toBeVisible();
  await jana.page.keyboard.press('Escape');
  const admin = await newAdmin(browser);
  await open(admin.page);
  await find(admin.page, 'new project');
  await expect(options(admin.page).first()).toContainText('New project');
  await box(admin.page).fill('admin: audit');
  await expect(options(admin.page).first()).toContainText('Admin: Audit log');
  await admin.page.keyboard.press('Enter');
  await expect(admin.page).toHaveURL(/#\/admin\/audit$/);
  await jana.ctx.close(); await admin.ctx.close();
});

test('PAL-04: issues and projects the person cannot see, trashed and archived issues are never listed', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const secret = await call(jana.page, 'POST', '/api/v1/issues', { project: 'XYZ', type: 'Task', summary: 'PAL-04 hidden from the viewer' });
  const trashed = await createIssue(jana.page, 'PAL-04 trashed', 'x');
  const archived = await createIssue(jana.page, 'PAL-04 archived', 'x');
  const visible = await createIssue(jana.page, 'PAL-04 visible', 'x');
  expect((await call(jana.page, 'POST', '/api/v1/issues/' + trashed + '/lifecycle', { op: 'delete' })).status).toBe(200);
  const resolve = (await call(jana.page, 'GET', '/api/v1/issues/' + archived + '/view')).data.transitions.find(t => t.to_category === 'done');
  expect((await call(jana.page, 'POST', '/api/v1/issues/' + archived + '/transitions', { transition: resolve.id })).status).toBe(200); // only finished work is archived
  expect((await call(jana.page, 'POST', '/api/v1/issues/' + archived + '/archive', {})).status).toBe(200);
  const viewer = await newUser(browser, 'viewer@example.com'); // a viewer of ABC only
  await open(viewer.page);
  await box(viewer.page).fill('xyz');
  await expect(viewer.page.getByRole('listbox', { name: 'Results' })).not.toContainText('Xray');
  for (const k of [secret.data.key, trashed, archived]) {
    await box(viewer.page).fill(k);
    await viewer.page.waitForTimeout(600); // past the debounce and the answer
    await expect(viewer.page.getByRole('listbox', { name: 'Results' }), k).not.toContainText(k);
  }
  await find(viewer.page, visible);
  await expect(options(viewer.page).first()).toContainText(visible);
  await jana.ctx.close(); await viewer.ctx.close();
});

test('PAL-05: typing fast sends one request per pause, and a slow older answer never replaces a newer one', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const a = await createIssue(page, 'PAL-05 alpha', 'x');
  await page.goto('/');
  const seen = [];
  await page.route('**/api/v1/search/typeahead*', async route => {
    const q = new URL(route.request().url()).searchParams.get('q');
    seen.push(q);
    if (q === 'ABC') await new Promise(r => setTimeout(r, 1200)); // the older question is answered last
    await route.continue();
  });
  await open(page);
  await box(page).pressSequentially('ABC', { delay: 20 });
  await page.waitForTimeout(300); // the first question goes out
  await box(page).pressSequentially('-' + a.split('-')[1], { delay: 20 });
  await expect(options(page).first()).toContainText(a + ' PAL-05 alpha');
  await page.waitForTimeout(1500); // the slow answer for "ABC" arrives now
  await expect(options(page).first(), 'a stale answer replaced the current one').toContainText(a + ' PAL-05 alpha');
  expect(seen, 'one request per pause in typing').toEqual(['ABC', a]);
  await ctx.close();
});

for (const theme of ['dark', 'light']) {
  test(`PAL-06: keyboard and screen reader: combobox and listbox, arrows, Enter, Escape, recent first (${theme})`, async ({ browser }) => {
    const { page, ctx } = await newUser(browser, 'jana@example.com');
    await page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    const key = await createIssue(page, 'PAL-06 recent ' + theme, 'x');
    await page.goto('/');
    await open(page);
    await expect(box(page)).toHaveAttribute('aria-controls', 'pal-list');
    await expect(box(page)).toHaveAttribute('aria-activedescendant', 'pal-0');
    await expect(options(page).first()).toHaveAttribute('aria-selected', 'true');
    await page.keyboard.press('ArrowDown');
    await expect(box(page)).toHaveAttribute('aria-activedescendant', 'pal-1');
    await expect(options(page).nth(1)).toHaveAttribute('aria-selected', 'true');
    await page.keyboard.press('ArrowUp');
    await expect(box(page)).toHaveAttribute('aria-activedescendant', 'pal-0');
    await axe(page, 'palette');
    await find(page, key);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(new RegExp(key + '$'));
    await open(page); // empty: the issue just opened is first
    await expect(options(page).first()).toContainText('Recent');
    await expect(options(page).first()).toContainText(key);
    // Tab stays in the dialog, Escape closes it and gives focus back
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => !!document.activeElement.closest('[role=dialog]'))).toBe(true);
    await page.keyboard.press('Escape');
    await expect(box(page)).toHaveCount(0);
    await ctx.close();
  });
}

test('PAL-07: the palette opens and shows its first results quickly', async ({ browser }) => {
  const { page, ctx } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'PAL-07 timing', 'x');
  await page.goto('/');
  await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
  const t0 = Date.now();
  await page.keyboard.press('Control+k');
  await expect(options(page).first()).toBeVisible();
  const opened = Date.now() - t0;
  const t1 = Date.now();
  await box(page).fill(key);
  await expect(options(page).first()).toContainText(key);
  const answered = Date.now() - t1; // includes the 150 ms pause before asking
  console.log(`PAL-07: open ${opened} ms, first matching result ${answered} ms`);
  expect(opened).toBeLessThan(300);
  expect(answered).toBeLessThan(150 + 150 + 200); // the pause, the typeahead budget (SPEC 21) and the test driver's own overhead
  await ctx.close();
});
