import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

test('CORE-12: ask someone to approve; the approver decides from the waiting list; the thread and the outcome show', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'Needs sign-off ' + Math.random().toString(36).slice(2, 7), 'please approve');
  const p = jana.page;
  await p.goto('/#/issue/' + key);
  await p.getByLabel('Approvers', { exact: true }).fill('arkady@example.com');
  await p.getByLabel('Approval rule').selectOption('any');
  await p.getByLabel('Note to the approvers').fill('ship it?');
  await p.getByRole('button', { name: 'Ask for approval' }).click();
  const card = p.getByRole('region', { name: 'Approvals' });
  await expect(card).toContainText('Waiting');
  await expect(card).toContainText('one approval is enough');
  await expect(card).toContainText('not decided yet');
  // the asker has no approve button: it is not theirs to press
  await expect(card.getByRole('button', { name: 'Approve' })).toHaveCount(0);
  // the approver finds it on their waiting list and approves with a comment that is markup-free text
  const a = ark.page;
  await a.goto('/#/approvals');
  const row = a.getByRole('listitem').filter({ hasText: key });
  await expect(row).toBeVisible();
  const axe = await new AxeBuilder({ page: a }).analyze();
  expect(axe.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await row.getByLabel('Comment on ' + key).fill('looks fine <b>to me</b>');
  await row.getByRole('button', { name: 'Approve ' + key }).click();
  await expect(a.getByRole('listitem').filter({ hasText: key })).toHaveCount(0);
  // the decision is on the ticket and in its thread
  await p.reload();
  await expect(p.getByRole('region', { name: 'Approvals' })).toContainText('Approved');
  await expect(p.getByRole('region', { name: 'Approvals' })).toContainText('approved: looks fine');
  await expect(p.getByText('Approved: looks fine').first()).toBeVisible();
  // a granted approval takes no more decisions and a new request can be made
  await expect(p.getByRole('button', { name: 'Ask for approval' })).toBeVisible();
  const a11y = await new AxeBuilder({ page: p }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
});
