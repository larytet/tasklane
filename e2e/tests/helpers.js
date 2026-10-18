import { expect } from '@playwright/test';

export const PASSWORD = 'password123';

export async function login(page, email) {
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
}

// Any uncaught error in the page is a test failure: it is collected here and asserted in afterEach (see ui.spec and conflict.spec).
export const pageErrors = [];
export function watch(page) {
  page.on('pageerror', e => pageErrors.push(String(e)));
  page.on('console', m => { if (m.type() === 'error' && !/Failed to load resource/.test(m.text())) pageErrors.push('console.error: ' + m.text()); });
}

export async function newUser(browser, email, opts = {}) {
  const ctx = await browser.newContext(opts);
  ctx.on('page', watch);
  const page = await ctx.newPage();
  await login(page, email);
  return { ctx, page };
}

// A fresh issue per test so tests do not depend on each other.
export async function createIssue(page, summary, description) {
  const r = await page.evaluate(async ({ summary, description }) => {
    const res = await fetch('/api/v1/issues', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ project: 'ABC', type: 'Task', summary, description }) });
    return res.json();
  }, { summary, description });
  return r.key;
}

export async function saveAsApi(page, key, body, baseSeq) {
  return page.evaluate(async ({ key, body, baseSeq }) => {
    const res = await fetch(`/api/v1/issues/${key}/text/description`, { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify({ body, base_seq: baseSeq, save_id: Math.random().toString(36) }) });
    return { status: res.status, body: await res.json() };
  }, { key, body, baseSeq });
}

export async function versions(page, key) {
  return page.evaluate(async key => (await fetch(`/api/v1/issues/${key}/text/description/versions`, { credentials: 'same-origin' })).json(), key);
}
