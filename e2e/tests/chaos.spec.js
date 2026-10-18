import { test, expect } from '@playwright/test';
import { pageErrors, newUser, createIssue, saveAsApi } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

// CHAOS-06: the realtime layer is switched off on the server (its ticket endpoint answers 404 "realtime_disabled"). The page asks
// once and stops asking, editing and saving go through REST, and a save based on an old version still opens the conflict dialog.
test('CHAOS-06: with live updates switched off the page still edits, saves and catches conflicts', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const ark = await newUser(browser, 'arkady@example.com');
  const key = await createIssue(jana.page, 'No sockets', 'original text');
  let tickets = 0;
  await jana.page.route('**/api/v1/ws/ticket', route => {
    tickets++;
    return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'live updates are switched off on this server', code: 'realtime_disabled' }) });
  });
  await jana.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await jana.page.getByLabel('Description editor').fill('jana wrote this while the sockets were off');
  await jana.page.waitForTimeout(4000); // a page that kept retrying would have asked again by now (1 s, then 2 s, ...)
  expect(tickets).toBeLessThanOrEqual(1);
  // someone else saves first; Jana's save, from the version she started with, is caught
  await ark.page.goto('/');
  expect((await saveAsApi(ark.page, key, 'arkady saved first', 1)).status).toBe(200);
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByRole('dialog')).toContainText('Arkady M. saved changes');
});

// and a plain save with nobody else around works the same without the socket
test('CHAOS-06: a plain save works with the realtime layer off', async ({ browser }) => {
  const jana = await newUser(browser, 'jana@example.com');
  const key = await createIssue(jana.page, 'No sockets two', 'before');
  await jana.page.route('**/api/v1/ws/ticket', route => route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ code: 'realtime_disabled' }) }));
  await jana.page.goto(`/#/issue/${key}`);
  await jana.page.getByRole('button', { name: 'Edit' }).click();
  await jana.page.getByLabel('Description editor').fill('after, saved over REST only');
  await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(jana.page.getByText('after, saved over REST only')).toBeVisible();
});
