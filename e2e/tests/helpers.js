import { expect } from '@playwright/test';

export const PASSWORD = 'password123';

export async function login(page, email) {
  await page.goto('/');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Create new' })).toBeVisible();
}

// Any uncaught error in the page is a test failure: it is collected here and asserted in afterEach (see ui.spec and conflict.spec).
export const pageErrors = [];
export function watch(page) {
  page.on('pageerror', e => pageErrors.push(String(e)));
  page.on('console', m => { if (m.type() === 'error' && !/Failed to load resource/.test(m.text())) pageErrors.push('console.error: ' + m.text()); });
}

// Every context a test opens with newUser is closed after it (each spec's afterEach), so pages left open by earlier tests do not keep
// polling the server and slow the later ones down.
const openContexts = [];
export async function closeContexts() {
  while (openContexts.length) { const c = openContexts.pop(); try { await c.close(); } catch (e) { /* closed already */ } }
}

export async function newUser(browser, email, opts = {}) {
  const ctx = await browser.newContext(opts);
  openContexts.push(ctx);
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

// The search box speaks plain text by default (Text) and TQL when asked: switch to TQL and give back the box.
export async function tqlBox(page) {
  const btn = page.getByRole('button', { name: 'TQL', exact: true });
  if ((await btn.getAttribute('aria-pressed')) !== 'true') await btn.click();
  return page.getByLabel('Search in TQL');
}
