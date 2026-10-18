import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
const uniq = () => Math.random().toString(36).slice(2, 7);

test('CORE-10: a comment by someone else lands in the inbox, shows on the bell, and can be read and archived', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const sum = 'Inbox thread ' + uniq();
  const key = await createIssue(jana.page, sum, 'about inbox');
  // Jana (the reporter) hears when Arkady comments
  await ark.page.evaluate(async ({ key }) => {
    await fetch(`/api/v1/issues/${key}/comments`, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ body: 'please look at this inbox item', internal: false }) });
  }, { key });
  await jana.page.goto('/#/inbox/unread');
  await expect(jana.page.getByRole('heading', { name: 'Notifications' })).toBeVisible();
  const row = jana.page.getByRole('listitem').filter({ hasText: key });
  await expect(row).toBeVisible({ timeout: 15000 }); // the notifier runs every second
  await expect(row).toContainText('please look at this inbox item');
  await expect(jana.page.getByRole('button', { name: /^Inbox, \d+ unread$/ })).toBeVisible();
  const a11y = await new AxeBuilder({ page: jana.page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  // reading it takes it off the unread list; archiving takes it off the default list
  await row.getByRole('button', { name: `Mark ${key} notification read` }).click();
  await expect(jana.page.getByRole('listitem').filter({ hasText: key })).toHaveCount(0);
  await jana.page.getByRole('link', { name: 'All' }).click();
  const all = jana.page.getByRole('listitem').filter({ hasText: key });
  await expect(all).toBeVisible();
  await all.getByRole('button', { name: `Archive ${key} notification` }).click();
  await expect(jana.page.getByRole('listitem').filter({ hasText: key })).toHaveCount(0);
  await jana.page.getByRole('link', { name: 'Archived' }).click();
  await expect(jana.page.getByRole('listitem').filter({ hasText: key })).toBeVisible();
  // the commenter is not told about their own comment
  await ark.page.goto('/#/inbox');
  await expect(ark.page.getByRole('heading', { name: 'Notifications' })).toBeVisible();
  await expect(ark.page.getByRole('listitem').filter({ hasText: 'please look at this inbox item' })).toHaveCount(0);
});

test('CORE-10: watching from the ticket and notification preferences', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(ark.page, 'Watch me ' + uniq(), 'watching');
  await jana.page.goto('/#/issue/' + key);
  const btn = jana.page.getByRole('button', { name: /^Watch/ });
  await expect(btn).toHaveText('Watch');
  await btn.click();
  await expect(jana.page.getByRole('button', { name: 'Watching ✓' })).toHaveAttribute('aria-pressed', 'true');
  await jana.page.reload();
  await expect(jana.page.getByRole('button', { name: 'Watching ✓' })).toBeVisible();
  // preferences dialog saves and reloads
  await jana.page.goto('/#/inbox');
  await jana.page.getByRole('button', { name: 'Preferences' }).click();
  const dlg = jana.page.getByRole('dialog');
  await dlg.getByLabel('Description edits').uncheck();
  await dlg.getByLabel('Quiet hours start').fill('22:00');
  await dlg.getByLabel('Quiet hours end').fill('07:00');
  await dlg.getByLabel('Time zone for quiet hours').fill('Europe/Berlin');
  const a11y = await new AxeBuilder({ page: jana.page }).analyze();
  expect(a11y.violations.filter(v => ['serious', 'critical'].includes(v.impact))).toEqual([]);
  await dlg.getByRole('button', { name: 'Save' }).click();
  await expect(jana.page.getByText('Preferences saved')).toBeVisible();
  await jana.page.getByRole('button', { name: 'Preferences' }).click();
  await expect(jana.page.getByRole('dialog').getByLabel('Description edits')).not.toBeChecked();
  await expect(jana.page.getByRole('dialog').getByLabel('Quiet hours start')).toHaveValue('22:00');
  // a bad time zone is refused with a message
  await jana.page.getByRole('dialog').getByLabel('Time zone for quiet hours').fill('Mars/Olympus');
  await jana.page.getByRole('dialog').getByRole('button', { name: 'Save' }).click();
  await expect(jana.page.getByRole('dialog').getByRole('alert')).toContainText('time zone');
});
