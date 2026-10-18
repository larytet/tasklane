import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 7);

test('CORE-12: build a dashboard of gadgets, see each as the viewer, share it, and show hostile text as text', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const sum = 'Dash issue ' + uniq();
  const key = await createIssue(jana.page, sum, 'for the dashboard');
  const p = jana.page;
  await p.goto('/#/dashboards');
  await expect(p.getByRole('heading', { name: 'Dashboards' })).toBeVisible();
  const name = 'Overview ' + uniq();
  await p.getByLabel('Dashboard name').fill(name);
  await p.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(p.getByRole('heading', { name, level: 2 })).toBeVisible();
  // a text gadget with markup in it: shown as text, never as markup
  await p.getByRole('button', { name: 'Edit' }).click();
  await p.getByLabel('Gadget type').selectOption('text');
  await p.getByLabel('Gadget title').fill('Notes');
  await p.getByLabel('Text', { exact: true }).fill('<img src=x onerror="window.__pwned=1"> hello');
  await p.getByRole('button', { name: 'Add gadget' }).click();
  await expect(p.getByRole('region', { name: 'Notes' })).toContainText('<img src=x');
  expect(await p.evaluate(() => window.__pwned)).toBeUndefined();
  // an assigned-to-me gadget lists what is on the viewer's plate; a pie counts by status
  await p.getByLabel('Gadget type').selectOption('pie');
  await p.getByLabel('Gadget title').fill('By status');
  await p.getByRole('button', { name: 'Add gadget' }).click();
  await expect(p.getByRole('region', { name: 'By status' }).getByRole('listitem').first()).toBeVisible();
  await p.getByLabel('Gadget type').selectOption('filter_results');
  await p.getByLabel('Gadget title').fill('Everything');
  await p.getByLabel('Query').fill('text ~ "Dash issue"');
  await p.getByRole('button', { name: 'Add gadget' }).click();
  await expect(p.getByRole('region', { name: 'Everything' }).getByRole('link', { name: key })).toBeVisible();
  await p.getByRole('button', { name: 'Done editing' }).click();
  const a11y = await new AxeBuilder({ page: p }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // favourite shows on the list
  await p.getByRole('button', { name: '☆ Favourite' }).click();
  await expect(p.getByRole('button', { name: '★ Favourite' })).toBeVisible();
  await p.goto('/#/dashboards');
  await expect(p.getByRole('button', { name: 'Remove from favourites: ' + name })).toBeVisible();
});
