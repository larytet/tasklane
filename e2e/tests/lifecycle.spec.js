import { test, expect } from '@playwright/test';
import { pageErrors, newUser, createIssue, tqlBox } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

async function openActions(page, item) {
  await page.getByRole('button', { name: /Actions/ }).click();
  await page.getByRole('button', { name: item }).click();
}

test('LIFE-09/10, UNDO-06: a delete with nothing blocking or dropped goes to the trash at once with Undo, and is restorable from there', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'Trash me ' + Math.random().toString(36).slice(2, 6), 'body');
  await jana.page.goto(`/#/issue/${key}`);
  await openActions(jana.page, /Delete/);
  // its plan (checked by a dry run) has no blockers and drops nothing, so no plan dialog: the toast offers Undo instead (SPEC 4.6)
  await expect(jana.page.getByRole('button', { name: 'Create new' })).toBeVisible();
  await expect(jana.page.locator('#toasts')).toContainText(key + ' moved to the trash.');
  await expect(jana.page.locator('#toasts').getByRole('button', { name: 'Undo' })).toBeVisible();
  await expect(jana.page.getByRole('dialog')).toHaveCount(0);
  // gone from lists and search, gone (410) when opened directly
  await (await tqlBox(jana.page)).fill(`key = ${key}`);
  await (await tqlBox(jana.page)).press('Enter');
  await expect(jana.page.getByText('No issues match.')).toBeVisible();
  await jana.page.goto(`/#/issue/${key}`);
  await expect(jana.page.getByRole('alert')).toContainText('This issue was deleted');
  // the trash lists it, and restoring brings it back with its key
  await jana.page.getByRole('link', { name: 'Open the trash' }).click();
  await expect(jana.page.getByRole('heading', { name: 'Trash' })).toBeVisible();
  await expect(jana.page.getByRole('listitem').filter({ hasText: key })).toContainText('deleted by Jana K.');
  await jana.page.getByRole('listitem').filter({ hasText: key }).getByRole('button', { name: 'Restore' }).click();
  await expect(jana.page.getByText('Restored ' + key)).toBeVisible();
  await jana.page.goto(`/#/issue/${key}`);
  await expect(jana.page.getByRole('heading', { level: 1 })).toBeVisible();
});

test('LIFE-09/18: a deletion reaches the other person with the editor open; the draft is kept and a save is refused with 410', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Deleted under an editor', 'original');
  for (const u of [jana, ark]) await u.page.goto(`/#/issue/${key}`);
  await ark.page.getByRole('button', { name: 'Edit' }).click();
  await ark.page.getByLabel('Description editor').fill('words typed while it is being deleted');
  await openActions(jana.page, /Delete/); // nothing blocks it, so it goes at once (UNDO-06)
  await expect(ark.page.getByRole('alert').filter({ hasText: 'This issue was deleted' })).toBeVisible();
  await expect(ark.page.getByLabel('Description editor')).toHaveValue('words typed while it is being deleted'); // never lost
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(ark.page.getByText(/your text is kept in your draft/)).toBeVisible();
});

test('LIFE-05: moving gives a new key; the old key still opens the issue and the address bar follows', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'Moving ' + Math.random().toString(36).slice(2, 6), 'text');
  await jana.page.goto(`/#/issue/${key}`);
  await openActions(jana.page, /Move/);
  const dlg = jana.page.getByRole('dialog');
  await expect(dlg).toContainText('keeps working as an alias forever');
  await dlg.getByRole('button', { name: 'Apply' }).click();
  await expect(jana.page.getByText(/Moved to XYZ-\d+/)).toBeVisible();
  await expect(jana.page).toHaveURL(/#\/issue\/XYZ-\d+/);
  const newKey = (await jana.page.url()).match(/XYZ-\d+/)[0];
  // the old key resolves in the browser and the URL is corrected to the new key
  await jana.page.goto('/?r=1');
  await jana.page.goto(`/#/issue/${key}`);
  await expect(jana.page.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(jana.page).toHaveURL(new RegExp('#/issue/' + newKey));
  // search by the old key finds it
  await expect(async () => {
    await jana.page.goto('/#/');
    await (await tqlBox(jana.page)).fill(`key = ${key}`);
    await (await tqlBox(jana.page)).press('Enter');
    await expect(jana.page.getByRole('listitem')).toHaveCount(1, { timeout: 1500 });
  }).toPass({ timeout: 15000 });
});

test('LIFE-05/07: a person with the old key open follows the issue when it moves', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Followed', 'text');
  for (const u of [jana, ark]) await u.page.goto(`/#/issue/${key}`);
  await openActions(jana.page, /Move/);
  await jana.page.getByRole('dialog').getByRole('button', { name: 'Apply' }).click();
  await expect(ark.page).toHaveURL(/#\/issue\/XYZ-\d+/);   // the open page followed the issue
  await expect(ark.page.getByText(/moved this issue to XYZ-\d+/)).toBeVisible();
  await expect(ark.page.getByRole('heading', { level: 1 })).toBeVisible();
});

test('LIFE-08: clone shows what it will do, creates the copy and opens it', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const title = 'Cloneable ' + Math.random().toString(36).slice(2, 6);
  const key = await createIssue(jana.page, title, 'copy this text');
  await jana.page.goto(`/#/issue/${key}`);
  await openActions(jana.page, /Clone/);
  const dlg = jana.page.getByRole('dialog');
  await expect(dlg).toContainText('as ABC-');
  await dlg.getByRole('button', { name: 'Apply' }).click();
  await expect(jana.page.getByLabel('Summary')).toHaveValue('CLONE - ' + title);
  await expect(jana.page.getByText('copy this text')).toBeVisible();
});

test('LIFE-04: converting the type is planned and applied, and a blocker explains why it cannot run', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'Convertible', 'text');
  await jana.page.goto(`/#/issue/${key}`);
  await openActions(jana.page, /Change issue type/);
  let dlg = jana.page.getByRole('dialog');
  await dlg.getByLabel('New type').selectOption('Subtask');
  await expect(dlg).toContainText('needs a parent');          // a blocker, with the reason
  await expect(dlg.getByRole('button', { name: 'Apply' })).toBeDisabled();
  await dlg.getByLabel('New type').selectOption('Bug');
  await expect(dlg).toContainText('from Task to Bug');
  await dlg.getByRole('button', { name: 'Apply' }).click();
  await expect(jana.page.getByText('Bug · reported by')).toBeVisible();
});

test('lifecycle: viewers have no Actions menu', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const vera = await newUser(browser, 'viewer@example.com');
  const key = await createIssue(jana.page, 'No actions for viewers', 'text');
  await vera.page.goto(`/#/issue/${key}`);
  await expect(vera.page.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(vera.page.getByRole('button', { name: /Actions/ })).toHaveCount(0);
});
