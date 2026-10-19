import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pageErrors, newUser } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const uniq = () => Math.random().toString(36).slice(2, 7);
const openCreate = async page => {
  await page.getByRole('button', { name: 'Create new' }).click();
  await page.getByRole('menuitem', { name: 'New issue' }).click();
  await expect(page.getByRole('dialog', { name: 'Create issue' })).toBeVisible();
};
const names = async (page, key) => (await (await page.request.get('/api/v1/issues/' + key + '/view')).json()).attachments.map(a => a.name).sort();

test('ATT-10: files of any type picked in the Create issue dialog are listed, removable, and uploaded to the new issue', async ({ browser }) => {
  const { ctx, page } = await newUser(browser, 'jana@example.com');
  await openCreate(page);
  await page.getByLabel('Summary').fill('With files ' + uniq());
  await page.getByLabel('Attach files').setInputFiles([
    { name: 'data.bin', mimeType: 'application/octet-stream', buffer: Buffer.from([0, 1, 2, 3, 250, 251]) },
    { name: 'drop-me.csv', mimeType: 'text/csv', buffer: Buffer.from('a,b\n1,2\n') },
  ]);
  // a file from disk, chosen twice, is listed once (the same name, size and modified time)
  const onDisk = join(mkdtempSync(join(tmpdir(), 'att-')), 'notes.txt');
  writeFileSync(onDisk, 'hello');
  await page.getByLabel('Attach files').setInputFiles(onDisk);
  await page.getByLabel('Attach files').setInputFiles(onDisk);
  const list = page.getByRole('list', { name: 'Files to attach' });
  await expect(list.getByRole('listitem')).toHaveCount(3);
  await expect(list).toContainText('notes.txt');
  await page.getByRole('button', { name: 'Remove drop-me.csv' }).click();
  await expect(list.getByRole('listitem')).toHaveCount(2);
  const axe = await new AxeBuilder({ page }).analyze();
  expect(axe.violations.filter(v => ['critical', 'serious'].includes(v.impact))).toEqual([]);
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/#\/issue\/[A-Z0-9]+-\d+$/);
  const key = page.url().split('/').pop();
  await expect(page.getByText('data.bin').first()).toBeVisible();
  expect(await names(page, key)).toEqual(['data.bin', 'notes.txt']);
  await ctx.close();
});

test('ATT-11: files dropped on the Create issue dialog are added; a drag that carries no file is ignored; a large file goes by the chunked path', async ({ browser }) => {
  const { ctx, page } = await newUser(browser, 'jana@example.com');
  await openCreate(page);
  await page.getByLabel('Summary').fill('Dropped files ' + uniq());
  const drop = (type, files) => page.evaluate(({ type, files }) => {
    const dt = new DataTransfer();
    for (const f of files) dt.items.add(new File([f.size ? new Uint8Array(f.size) : f.text], f.name, { type: f.mime }));
    document.querySelector('.scrim').dispatchEvent(new DragEvent(type, { dataTransfer: dt, bubbles: true, cancelable: true }));
    return document.querySelector('.dialog').classList.contains('dropping');
  }, { type, files });
  expect(await drop('dragover', [{ name: 'a.pdf', text: '%PDF-1.4', mime: 'application/pdf' }])).toBe(true); // the dialog shows it will take the drop
  await drop('drop', [{ name: 'a.pdf', text: '%PDF-1.4', mime: 'application/pdf' }, { name: 'b.zip', text: 'PK', mime: 'application/zip' }, { name: 'big.dat', size: 9 << 20, mime: 'application/octet-stream' }]);
  await expect(page.getByRole('list', { name: 'Files to attach' }).getByRole('listitem')).toHaveCount(3);
  await expect(page.locator('.dialog.dropping')).toHaveCount(0);
  // text dragged in (no file) adds nothing
  await page.evaluate(() => { const dt = new DataTransfer(); dt.setData('text/plain', 'x'); document.querySelector('.scrim').dispatchEvent(new DragEvent('drop', { dataTransfer: dt, bubbles: true, cancelable: true })); });
  await expect(page.getByRole('list', { name: 'Files to attach' }).getByRole('listitem')).toHaveCount(3);
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/#\/issue\/[A-Z0-9]+-\d+$/, { timeout: 60000 });
  const key = page.url().split('/').pop();
  await expect.poll(() => names(page, key), { timeout: 30000 }).toEqual(['a.pdf', 'b.zip', 'big.dat']);
  await ctx.close();
});
