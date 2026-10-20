import { test, expect } from '@playwright/test';
import { pageErrors, newUser, createIssue, versions, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const DOC = 'Steps:\n\n1. Use test card\n\nExpected: order created\n\nNotes: see runbook';

async function setup(browser) {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Conflict ' + Math.random().toString(36).slice(2, 6), DOC);
  for (const u of [jana, ark]) await u.page.goto(`/#/issue/${key}`);
  for (const u of [jana, ark]) { await u.page.getByRole('button', { name: 'Edit' }).click(); await expect(u.page.getByLabel('Description editor')).toBeVisible(); }
  return { jana, ark, key };
}

test('LIVE-02/03: non-overlapping edits merge automatically and both versions stay in history', async ({ browser }) => {
  const { jana, ark, key } = await setup(browser);
  await jana.page.getByLabel('Description editor').fill(DOC.replace('1. Use test card', '1. Use test card 4000'));
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('1. Use test card 4000')).toBeVisible();
  // Arkady's page learns of it over the socket: a banner appears while his editor stays untouched
  await expect(ark.page.getByText(/The description was changed by Jana K\./)).toBeVisible();
  await expect(ark.page.getByLabel('Description editor')).toHaveValue(DOC);
  await ark.page.getByLabel('Description editor').fill(DOC.replace('Notes: see runbook', 'Notes: see the runbook and the FAQ'));
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  const dlg = ark.page.getByRole('dialog');
  await expect(dlg).toContainText('Jana K. saved changes to the description while you were editing');
  await expect(dlg).toContainText('0 blocks changed by both');
  const merged = dlg.getByLabel('Merged result');
  await expect(merged).toHaveValue('Steps:\n\n1. Use test card 4000\n\nExpected: order created\n\nNotes: see the runbook and the FAQ');
  await dlg.getByRole('button', { name: 'Save merged result' }).click();
  await expect(ark.page.getByRole('dialog')).toHaveCount(0);
  await expect(ark.page.getByText('1. Use test card 4000')).toBeVisible();
  await expect(ark.page.getByText('Notes: see the runbook and the FAQ')).toBeVisible();
  const vs = await versions(ark.page, key);
  expect(vs.map(v => v.mode)).toEqual(['merge', 'save', 'create']);
  expect(vs.map(v => v.actor)).toEqual(['Arkady M.', 'Jana K.', 'Jana K.']);
});

test('LIVE-03: overlapping edits are chosen per block; the choice is what gets saved', async ({ browser }) => {
  const { jana, ark } = await setup(browser);
  await jana.page.getByLabel('Description editor').fill(DOC.replace('Expected: order created', 'Expected: order paid'));
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('Expected: order paid')).toBeVisible();
  await ark.page.getByLabel('Description editor').fill(DOC.replace('Expected: order created', 'Expected: order created and receipt emailed'));
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  const dlg = ark.page.getByRole('dialog');
  await expect(dlg).toContainText('1 block changed by both');
  const group = dlg.getByRole('group', { name: /Conflicting block/ });
  await expect(group).toBeVisible();
  await expect(dlg.getByLabel('Merged result')).toHaveValue(/receipt emailed/); // defaults to mine
  await group.getByLabel('Use theirs').check();
  await expect(dlg.getByLabel('Merged result')).toHaveValue(/order paid/);
  await expect(dlg.getByLabel('Merged result')).not.toHaveValue(/receipt emailed/);
  await dlg.getByRole('button', { name: 'Save merged result' }).click();
  await expect(ark.page.getByRole('dialog')).toHaveCount(0);
  await expect(ark.page.getByText('Expected: order paid')).toBeVisible();
});

test('LIVE-03: keep mine overwrites, and the overwritten version is restorable from history', async ({ browser }) => {
  const { jana, ark, key } = await setup(browser);
  await jana.page.getByLabel('Description editor').fill('Jana rewrote everything');
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('Jana rewrote everything')).toBeVisible();
  await ark.page.getByLabel('Description editor').fill('Arkady rewrote everything');
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  await ark.page.getByRole('dialog').getByRole('button', { name: 'Keep mine' }).click();
  await expect(ark.page.getByRole('dialog')).toHaveCount(0);
  await expect(ark.page.getByText('Arkady rewrote everything')).toBeVisible();
  const vs = await versions(ark.page, key);
  expect(vs[0].mode).toBe('keep_mine');
  expect(vs.some(v => v.body === 'Jana rewrote everything')).toBe(true);
});

test('LIVE-03: keep theirs discards mine from the page but keeps it as a draft; a third save repeats the dialog', async ({ browser }) => {
  const { jana, ark } = await setup(browser);
  await jana.page.getByLabel('Description editor').fill('first from Jana');
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('first from Jana')).toBeVisible();
  await ark.page.getByLabel('Description editor').fill('Arkady text');
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  const dlg = ark.page.getByRole('dialog');
  await expect(dlg).toBeVisible();
  // while the dialog is open, Jana saves again: saving the merge must open the dialog again, never overwrite
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await jana.page.getByLabel('Description editor').fill('second from Jana');
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('second from Jana')).toBeVisible();
  await dlg.getByRole('button', { name: 'Save merged result' }).click();
  await expect(ark.page.getByRole('dialog')).toContainText('Jana K. saved changes');
  await expect(ark.page.getByRole('dialog').getByLabel('Merged result')).toBeVisible();
  await ark.page.getByRole('dialog').getByRole('button', { name: 'Keep theirs' }).click();
  await expect(ark.page.getByRole('dialog')).toHaveCount(0);
  await expect(ark.page.getByText('second from Jana')).toBeVisible();
  await expect(ark.page.getByText(/Restored your unsaved draft/)).toBeVisible(); // my text is not lost
});

test('A11Y-02: the conflict dialog is keyboard operable, traps focus and is not dismissed by Escape', async ({ browser }) => {
  const { jana, ark } = await setup(browser);
  await jana.page.getByLabel('Description editor').fill('other change');
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('other change')).toBeVisible();
  await ark.page.getByLabel('Description editor').fill('my change');
  await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
  const dlg = ark.page.getByRole('dialog');
  await expect(dlg).toBeVisible();
  await ark.page.keyboard.press('Escape');
  await expect(dlg).toBeVisible(); // a conflict is never dismissed by accident
  // Tab never leaves the dialog
  for (let i = 0; i < 12; i++) {
    await ark.page.keyboard.press('Tab');
    expect(await ark.page.evaluate(() => !!document.activeElement.closest('[role=dialog]'))).toBe(true);
  }
  for (let i = 0; i < 12; i++) {
    await ark.page.keyboard.press('Shift+Tab');
    expect(await ark.page.evaluate(() => !!document.activeElement.closest('[role=dialog]'))).toBe(true);
  }
  await dlg.getByRole('button', { name: 'Save merged result' }).focus();
  await ark.page.keyboard.press('Enter');
  await expect(ark.page.getByRole('dialog')).toHaveCount(0);
});
