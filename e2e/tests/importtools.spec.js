import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, watch } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

const ASANA = 'Task ID,Created At,Completed At,Last Modified,Name,Section/Column,Assignee Email,Tags,Notes,Parent task\n' +
  '1,2026-01-02,,2026-01-05,Prepare the report,Doing,ana@x.example,"finance, q1",Numbers for Q1,\n' +
  '2,2026-01-02,2026-01-04,2026-01-04,Collect the invoices,Done,,,,Prepare the report\n';

test('IMP: import an Asana export from the screen: a dry run changes nothing, the real import does, and it can be rolled back', async ({ browser }) => {
  const ctx = await browser.newContext();
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await page.goto('/');
  await page.getByLabel('Email').fill('admin@example.com');
  await page.getByLabel('Password').fill('adminpass1');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  const call = async (method, path, data) => { const r = await ctx.request.fetch(path, { method, headers: H, data }); let b = null; try { b = await r.json(); } catch (e) { /* none */ } return { status: r.status(), body: b }; };
  const pk = 'I' + uniq().toUpperCase();
  expect((await call('POST', '/api/v1/projects', { key: pk, name: 'Imports' })).status).toBe(201);
  const countIssues = async () => (await call('GET', '/api/v1/issues?q=' + encodeURIComponent('project = ' + pk))).body.total;

  await page.reload(); // the page learns of the new project
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.getByText('More', { exact: true }).click();
  await page.getByRole('link', { name: 'Import', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Import from another tool' })).toBeVisible();
  await page.getByLabel('Project', { exact: true }).selectOption(pk);
  await page.getByLabel('Import from').selectOption('asana');
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // no file: says so
  await page.getByRole('button', { name: 'Check and import' }).click();
  await expect(page.getByRole('alert')).toContainText('Choose the export file');
  await page.getByLabel('The export file').setInputFiles({ name: 'asana.csv', mimeType: 'text/csv', buffer: Buffer.from(ASANA) });
  await page.getByLabel('Who is who').fill('ana@x.example = ana@x.example');
  // dry run
  await page.getByRole('button', { name: 'Check and import' }).click();
  await expect(page.getByRole('heading', { name: 'Dry run: nothing was changed' })).toBeVisible();
  await expect(page.getByText(/issues created: 2/)).toBeVisible();
  expect(await countIssues()).toBe(0);
  // for real
  await page.getByRole('button', { name: 'Import for real' }).click();
  await expect(page.getByRole('heading', { name: 'Imported', exact: true })).toBeVisible();
  expect(await countIssues()).toBe(2);
  // rolled back
  await page.getByRole('button', { name: 'Roll this import back' }).click();
  await expect.poll(countIssues).toBe(0);
});
