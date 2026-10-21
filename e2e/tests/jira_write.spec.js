import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

// Jira write-through, as the page behaves (SPEC 18.1.1, WT-04, WT-08). There is no Jira in this run: the server's answers are those a
// project that writes through would give, put in place by request interception on a native issue, so what is under test is the page:
// the token dialog, the repeat of the held save, and the conflict message. The server side is covered against the Jira double in Go.

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const TOKEN_PAGE = 'https://id.atlassian.com/manage-profile/security/api-tokens';
const need = { error: 'saving to Jira needs your personal Jira API token', code: 'token_required', detail: { connection_id: 'c1', connection: 'acme', site_url: 'https://acme.atlassian.net', token_page: TOKEN_PAGE } };

test('WT-04: a save that needs a Jira token asks for it, says where it stays, and repeats the save once it is given', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'WT page ' + Date.now(), 'x');
  await page.goto('/#/issue/' + key);
  let patches = 0, tokenBody = null, tokenUrl = '';
  await page.route('**/api/v1/issues/' + key, async route => {
    if (route.request().method() !== 'PATCH') return route.fallback();
    patches++;
    if (patches === 1) return route.fulfill({ status: 428, contentType: 'application/json', body: JSON.stringify(need) });
    return route.fallback(); // the real server answers the repeat
  });
  await page.route('**/api/v1/jira/session-token', route => {
    tokenBody = route.request().postDataJSON(); tokenUrl = route.request().url();
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{"ok":true}' });
  });
  await page.getByLabel('Priority').selectOption('high');
  const dlg = page.getByRole('dialog', { name: 'Save to Jira as you' });
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText('kept only in memory for this session');
  await expect(dlg).toContainText('never written to disk or to the database');
  await expect(dlg).toContainText('disappears when you sign out or when Tasklane restarts');
  await expect(dlg.getByRole('link', { name: /Create an API token in Atlassian/ })).toHaveAttribute('href', TOKEN_PAGE);
  const tok = dlg.getByLabel('Jira API token');
  await expect(tok).toHaveAttribute('type', 'password');
  await expect(tok).toHaveAttribute('autocomplete', 'new-password');
  const ax = await new AxeBuilder({ page }).include('[role=dialog]').analyze();
  expect(ax.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => v.id)).toEqual([]);
  await tok.fill('pasted-token-123');
  await dlg.getByRole('button', { name: 'Save token' }).click();
  await expect(dlg).toBeHidden();
  await expect.poll(() => patches).toBe(2); // the held save was sent again, once
  expect(tokenBody).toEqual({ connection: 'c1', email: '', token: 'pasted-token-123' });
  expect(tokenUrl).not.toContain('pasted-token'); // never in a URL
  await expect(page.getByLabel('Priority')).toHaveValue('high');
});

test('WT-04: cancelling the dialog does not repeat the save and the person is told why', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'WT cancel ' + Date.now(), 'x');
  await page.goto('/#/issue/' + key);
  let patches = 0;
  await page.route('**/api/v1/issues/' + key, route => {
    if (route.request().method() !== 'PATCH') return route.fallback();
    patches++;
    return route.fulfill({ status: 428, contentType: 'application/json', body: JSON.stringify(need) });
  });
  await page.getByLabel('Priority').selectOption('low');
  const dlg = page.getByRole('dialog', { name: 'Save to Jira as you' });
  await expect(dlg).toBeVisible();
  await dlg.getByRole('button', { name: 'Cancel' }).click();
  await expect(dlg).toBeHidden();
  await expect(page.locator('.toast').filter({ hasText: 'personal Jira API token' })).toBeVisible();
  expect(patches).toBe(1);
});

test('WT-04: a token Jira refuses keeps the dialog open with Jira\'s reason, and Escape counts as no', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'WT refuse ' + Date.now(), 'x');
  await page.goto('/#/issue/' + key);
  await page.route('**/api/v1/issues/' + key, route => route.request().method() === 'PATCH' ? route.fulfill({ status: 428, contentType: 'application/json', body: JSON.stringify(need) }) : route.fallback());
  await page.route('**/api/v1/jira/session-token', route => route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: 'this token belongs to a different Jira account than yours', code: 'invalid' }) }));
  await page.getByLabel('Priority').selectOption('low');
  const dlg = page.getByRole('dialog', { name: 'Save to Jira as you' });
  await dlg.getByLabel('Jira API token').fill('someone-elses');
  await dlg.getByRole('button', { name: 'Save token' }).click();
  await expect(dlg.getByRole('alert')).toContainText('different Jira account');
  await expect(dlg.getByLabel('Jira API token')).toHaveValue(''); // a refused token is not left in the field
  await page.keyboard.press('Escape');
  await expect(dlg).toBeHidden();
  // the page is usable again: a later save asks again instead of waiting for a dialog that is gone
  await page.getByLabel('Priority').selectOption('high');
  await expect(page.getByRole('dialog', { name: 'Save to Jira as you' })).toBeVisible();
});

test('WT-08: a conflict with Jira says what changed and by whom, and Overwrite sends what the person has now seen', async ({ browser }) => {
  const { page } = await newUser(browser, 'jana@example.com');
  const key = await createIssue(page, 'WT conflict ' + Date.now(), 'x');
  // the page is told this issue's project writes through: the editors stay on and a save carries the base it started from
  await page.route('**/api/v1/issues/' + key + '/view', async route => {
    const res = await route.fetch();
    const body = await res.json();
    body.mirror = { site_url: 'https://acme.atlassian.net', jira_key: 'WT', issue_key: key, synced_at: new Date().toISOString(), lag_seconds: 1, stale: false, mode: 'write_through' };
    return route.fulfill({ response: res, json: body });
  });
  const bodies = [];
  await page.route('**/api/v1/issues/' + key, route => {
    if (route.request().method() !== 'PATCH') return route.fallback();
    bodies.push(route.request().postDataJSON());
    if (bodies.length === 1) {
      return route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ error: key + ' was changed in Jira since you opened it (priority); nothing was saved', code: 'jira_conflict',
        detail: { issue: key, fields: [{ field: 'priority', jira: 'highest', base: 'medium', current: 'highest', by: 'Other Person', at: '2026-10-21T08:00:00Z' }] } }) });
    }
    return route.fallback();
  });
  await page.goto('/#/issue/' + key);
  await expect(page.getByText('Your changes are saved to Jira as you')).toBeVisible();
  await page.getByLabel('Priority').selectOption('low');
  const toast = page.locator('.toast').filter({ hasText: 'Changed in Jira since you opened this' });
  await expect(toast).toContainText('priority is now "highest" (Other Person)');
  await expect(toast).toContainText('Nothing was saved');
  expect(bodies[0].base).toEqual({ priority: 'medium', assignee: '', labels: [] }); // what the form started from
  await toast.getByRole('button', { name: 'Overwrite' }).click();
  await expect.poll(() => bodies.length).toBe(2);
  expect(bodies[1].priority).toBe('low');
  expect(bodies[1].base.priority).toBe('highest'); // what the person has now been shown, so Jira's value is not compared with the old one again
});
