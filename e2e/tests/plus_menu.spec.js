import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, watch, PASSWORD } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 7);

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
const open = async page => { await page.getByRole('button', { name: 'Create new' }).click(); await expect(page.getByRole('menu', { name: 'Create new' })).toBeVisible(); };

test('PLUS-01: the + menu shows a member only what they may create; c opens New issue, but not while typing', async ({ browser }) => {
  const u = await newUser(browser, 'jana@example.com');
  const page = u.page;
  await open(page);
  const names = await page.getByRole('menuitem').allTextContents();
  expect(names).toEqual(['New issue', 'New dashboard']);
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['critical', 'serious'].includes(v.impact))).toEqual([]);
  await page.keyboard.press('Escape'); // closes the menu and returns to the button
  await expect(page.getByRole('menu')).toBeHidden();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeFocused();
  // c from the page
  await page.locator('h1, .brand').first().focus();
  await page.keyboard.press('c');
  await expect(page.getByRole('dialog', { name: 'Create issue' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toBeHidden();
  // c typed into the search box is text
  await page.getByRole('searchbox').first().focus();
  await page.keyboard.type('cat');
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(page.getByRole('searchbox').first()).toHaveValue('cat');
});

test('PLUS-02: an administrator makes a project, a dashboard, a user and an organization from the + menu', async ({ browser }) => {
  const { ctx, page } = await adminPage(browser);
  await open(page);
  expect(await page.getByRole('menuitem').allTextContents()).toEqual(['New issue', 'New project', 'New dashboard', 'New user', 'New organization']);

  // project: the key is validated by the server, a new one opens its issue list and is offered in New issue
  const key = 'P' + uniq().toUpperCase().replace(/[^A-Z0-9]/g, 'X');
  await page.getByRole('menuitem', { name: 'New project' }).click();
  await page.getByLabel('Project key').fill('1');
  await page.getByLabel('Project name').fill('Plus project');
  await page.getByRole('button', { name: 'Create project' }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'project key' })).toBeVisible();
  await page.getByLabel('Project key').fill(key.toLowerCase());
  await page.getByRole('button', { name: 'Create project' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(page).toHaveURL(new RegExp('project(%20|\\+)%3D(%20|\\+)' + key));
  await open(page);
  await page.getByRole('menuitem', { name: 'New issue' }).click();
  await expect(page.getByLabel('Project', { exact: true })).toContainText(key);
  await page.keyboard.press('Escape');
  // the same key again is refused with the server's message
  await open(page);
  await page.getByRole('menuitem', { name: 'New project' }).click();
  await page.getByLabel('Project key').fill(key);
  await page.getByLabel('Project name').fill('Again');
  await page.getByRole('button', { name: 'Create project' }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'already in use' })).toBeVisible();
  await page.keyboard.press('Escape');

  // dashboard
  const dash = 'Plus board ' + uniq();
  await open(page);
  await page.getByRole('menuitem', { name: 'New dashboard' }).click();
  await page.getByLabel('Dashboard name').fill(dash);
  await page.getByRole('button', { name: 'Create dashboard' }).click();
  await expect(page).toHaveURL(/#\/dashboards\/[^/]+$/);
  await expect(page.getByRole('heading', { name: dash })).toBeVisible();

  // user: can sign in with the password it was given
  const email = `plus${uniq()}@example.com`;
  await open(page);
  await page.getByRole('menuitem', { name: 'New user' }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Full name').fill('Plus User');
  await page.getByLabel('Initial password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Create user' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  const fresh = await newUser(browser, email);
  await expect(fresh.page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await fresh.ctx.close();

  // organization with its first administrator, who then sees the administrator items
  const org = 'Plus Org ' + uniq(), boss = `boss${uniq()}@org.example`;
  await open(page);
  await page.getByRole('menuitem', { name: 'New organization' }).click();
  await page.getByLabel('Organization name').fill(org);
  await page.getByLabel('Administrator email').fill(boss);
  await page.getByLabel('Administrator name').fill('Org Boss');
  await page.getByLabel('Administrator initial password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Create organization' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  const orgs = await (await ctx.request.get('/api/v1/orgs')).json();
  expect(orgs.map(o => o.name)).toContain(org);
  const b = await newUser(browser, boss);
  await open(b.page);
  // an organization administrator may add projects and users in their organization, but not make organizations
  expect(await b.page.getByRole('menuitem').allTextContents()).toEqual(['New issue', 'New project', 'New dashboard', 'New user']);
  await b.ctx.close();
  await ctx.close();
});
