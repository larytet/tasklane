import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, login, createIssue, tqlBox, PASSWORD } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(() => { expect(pageErrors, 'uncaught errors in the page').toEqual([]); });

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];

async function axe(page, what) {
  const r = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const bad = r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
  const detail = bad.map(v => `${v.id} (${v.impact}): ${v.help}\n  ${v.nodes.slice(0, 3).map(n => n.target.join(' ') + ' :: ' + (n.failureSummary || '').split('\n').slice(1, 3).join(' ')).join('\n  ')}`).join('\n');
  expect(bad, `${what}: serious or critical accessibility violations\n${detail}`).toEqual([]);
}

for (const theme of ['dark', 'light']) {
  test.describe(`A11Y-01 axe, ${theme} theme`, () => {
    test.beforeEach(async ({ page }) => { await page.addInitScript(t => { try { localStorage.setItem('tl-theme', t); } catch (e) { /* ignore */ } }, theme); });

    test('sign-in', async ({ page }) => { await page.goto('/'); await expect(page.getByLabel('Email')).toBeVisible(); await axe(page, 'sign-in'); });

    test('list, ticket, editor, create dialog', async ({ browser }) => {
      const u = await newUser(browser, 'jana@example.com');
      await u.page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
      await u.page.reload();
      await expect(u.page.getByRole('button', { name: '+ Create' })).toBeVisible();
      await axe(u.page, 'issue list');
      await u.page.getByRole('button', { name: '+ Create' }).click();
      await expect(u.page.getByRole('dialog')).toBeVisible();
      await axe(u.page, 'create dialog');
      await u.page.keyboard.press('Escape');
      await u.page.goto('/#/issue/ABC-1');
      await expect(u.page.getByRole('heading', { level: 1 })).toBeVisible();
      await axe(u.page, 'ticket');
      await u.page.getByRole('button', { name: 'Edit' }).click();
      await expect(u.page.getByLabel('Description editor')).toBeVisible();
      await axe(u.page, 'ticket with editor open');
    });

    test('conflict dialog', async ({ browser }) => {
      const jana = await newUser(browser, 'jana@example.com');
      const ark = await newUser(browser, 'arkady@example.com');
      for (const u of [jana, ark]) await u.page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
      const key = await createIssue(jana.page, 'axe conflict ' + theme, 'Steps:\n\nExpected: a\n\nNotes');
      for (const u of [jana, ark]) { await u.page.goto(`/?t=${theme}#/issue/${key}`); await u.page.getByRole('button', { name: 'Edit' }).click(); }
      await jana.page.getByLabel('Description editor').fill('Steps:\n\nExpected: b\n\nNotes');
      await jana.page.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(jana.page.getByText('Expected: b')).toBeVisible();
      await ark.page.getByLabel('Description editor').fill('Steps:\n\nExpected: c\n\nNotes');
      await ark.page.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(ark.page.getByRole('dialog')).toBeVisible();
      await axe(ark.page, 'conflict dialog');
    });
  });
}

test('UI-03: dark is the default, the stored choice wins, and a light user never sees a dark first paint', async ({ browser }) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto('/');
  expect(await page.evaluate(() => document.documentElement.dataset.theme)).toBe('dark');
  await login(page, 'jana@example.com');
  await page.getByRole('button', { name: 'Toggle theme' }).click();
  expect(await page.evaluate(() => document.documentElement.dataset.theme)).toBe('light');
  // Record the theme at the moment the parser first creates <body>, which is before anything is painted: with the head
  // script it is already light, without it the page is dark until app.js runs at the end of the body.
  await page.addInitScript(() => {
    window.__atBody = null;
    const obs = new MutationObserver(() => {
      if (document.body && window.__atBody === null) { window.__atBody = document.documentElement.dataset.theme || 'unset'; obs.disconnect(); }
    });
    obs.observe(document, { childList: true, subtree: true });
  });
  await page.reload();
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  expect(await page.evaluate(() => window.__atBody), 'the theme when <body> appears (before first paint)').toBe('light');
  expect(await page.evaluate(() => document.documentElement.dataset.theme)).toBe('light');
  // and the head script is what does it: it is a blocking script in <head>, before the stylesheet
  const html = await (await page.request.get('/')).text();
  expect(html.indexOf('/theme.js')).toBeGreaterThan(-1);
  expect(html.indexOf('/theme.js')).toBeLessThan(html.indexOf('</head>'));
  expect(html.indexOf('/theme.js')).toBeLessThan(html.indexOf('/app.css'));
});

for (const [name, w, h] of [['phone', 390, 800], ['tablet', 768, 1000], ['desktop', 1280, 800]]) {
  test(`UI-04 responsive: ${name} (${w}px) has no horizontal scroll and every control is reachable`, async ({ browser }) => {
    const u = await newUser(browser, 'jana@example.com', { viewport: { width: w, height: h } });
    const overflow = async () => u.page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
    for (const route of ['/', '/#/issue/ABC-1']) {
      await u.page.goto(route);
      await expect(u.page.getByRole('button', { name: '+ Create' })).toBeVisible();
      const o = await overflow();
      expect(o.sw, `${route} overflows at ${w}px: ${o.sw} > ${o.cw}`).toBeLessThanOrEqual(o.cw);
    }
    await expect(u.page.getByRole('button', { name: '+ Create' })).toBeInViewport();
    await expect(u.page.getByRole('button', { name: 'Sign out' })).toBeInViewport();
    await u.page.getByRole('button', { name: 'Edit' }).click();
    await expect(u.page.getByRole('button', { name: 'Save', exact: true })).toBeInViewport();
    // touch targets on the phone are at least 32 px (DESIGN 7)
    if (w < 600) {
      const small = await u.page.evaluate(() => [...document.querySelectorAll('button, select, input[type=text], .btn')].filter(e => e.offsetParent).map(e => ({ n: e.getAttribute('aria-label') || e.textContent.trim().slice(0, 20), h: e.getBoundingClientRect().height })).filter(x => x.h < 28));
      expect(small, 'controls smaller than 28 px').toEqual([]);
    }
  });
}

test('A11Y-02: create, open, comment and transition with the keyboard only', async ({ page }) => {
  await page.goto('/');
  await page.getByLabel('Email').focus();
  await page.keyboard.type('jana@example.com');
  await page.keyboard.press('Tab');
  await page.keyboard.type(PASSWORD);
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.getByRole('button', { name: '+ Create' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('dialog')).toBeVisible();
  // the dialog opens with focus inside it
  expect(await page.evaluate(() => !!document.activeElement.closest('[role=dialog]'))).toBe(true);
  const title = 'Keyboard issue ' + Math.random().toString(36).slice(2, 6);
  await page.getByPlaceholder('Summary').focus();
  await page.keyboard.type(title);
  await page.getByRole('button', { name: 'Create', exact: true }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(page.getByLabel('Summary')).toHaveValue(title);
  await page.getByPlaceholder('Reply…').focus();
  await page.keyboard.type('commented by keyboard');
  await page.getByRole('button', { name: 'Send' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('commented by keyboard')).toBeVisible();
  await page.getByLabel('Transition').focus();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(page.locator('.loz').first()).toHaveText('In Progress');
  // focus is always visible
  await page.getByRole('button', { name: 'Edit' }).focus();
  const ring = await page.evaluate(() => getComputedStyle(document.activeElement).outlineStyle);
  expect(ring).not.toBe('none');
  // the list rows open with Enter
  await page.getByText('Tasklane', { exact: true }).focus();
  await page.keyboard.press('Enter');
  await page.getByRole('listitem').first().focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
});

test('UI-05 and TQL in the UI: a search narrows the list; a bad query shows where it is wrong', async ({ browser }) => {
  const u = await newUser(browser, 'jana@example.com');
  const key = await createIssue(u.page, 'Zebra crossing bug', 'stripes');
  await u.page.goto('/');
  // search is derived data and may lag a saved change by a few seconds (SPEC 21): repeat the search until it appears
  await expect(async () => {
    await (await tqlBox(u.page)).fill('summary ~ "zebra"');
    await (await tqlBox(u.page)).press('Enter');
    await expect(u.page.getByRole('listitem')).toHaveCount(1, { timeout: 1500 });
  }).toPass({ timeout: 15000 });
  await expect(u.page.getByRole('listitem').first()).toContainText(key);
  await (await tqlBox(u.page)).fill('status = (');
  await (await tqlBox(u.page)).press('Enter');
  await expect(u.page.getByRole('alert')).toContainText('at position');
});

// the default box is plain text: words, a phrase, AND / OR / NOT, -word, and the same search shown as TQL when switched
test('SEARCH: the default search box takes plain text and can show it as TQL', async ({ browser }) => {
  const u = await newUser(browser, 'jana@example.com');
  const word = 'quokka' + Array.from({ length: 6 }, () => 'abcdefghijklmnopqrstuvwxyz'[Math.floor(Math.random() * 26)]).join('');
  const key = await createIssue(u.page, `${word} on the loose`, 'text about a small marsupial');
  await createIssue(u.page, `${word} draft note`, 'unrelated');
  await u.page.goto('/');
  const box = u.page.getByLabel('Search', { exact: true });
  await expect(async () => {
    await box.fill(`${word} -draft`);
    await box.press('Enter');
    await expect(u.page.getByRole('listitem')).toHaveCount(1, { timeout: 1500 });
  }).toPass({ timeout: 15000 });
  await expect(u.page.getByRole('listitem').first()).toContainText(key);
  await u.page.getByRole('button', { name: 'TQL', exact: true }).click(); // the search that ran, as TQL, to edit further
  await expect(u.page.getByLabel('Search in TQL')).not.toHaveValue('');
});

test.describe('UI-01 visual baselines', () => {
  for (const theme of ['dark', 'light']) {
    test(`screens, ${theme}`, async ({ browser }) => {
      const u = await newUser(browser, 'jana@example.com', { viewport: { width: 1100, height: 760 }, reducedMotion: 'reduce' });
      await u.page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
      await u.page.goto('/');
      await u.page.evaluate(t => { document.documentElement.dataset.theme = t; }, theme);
      const mask = () => [u.page.locator('.muted'), u.page.locator('.activity')];
      await u.page.goto('/#/issue/ABC-1');
      await expect(u.page.getByRole('heading', { level: 1 })).toBeVisible();
      await u.page.waitForTimeout(400);
      await expect(u.page).toHaveScreenshot(`ticket-${theme}.png`, { mask: mask() });
      await u.page.getByRole('button', { name: 'Edit' }).click();
      await expect(u.page.getByLabel('Description editor')).toBeVisible();
      await expect(u.page).toHaveScreenshot(`ticket-editor-${theme}.png`, { mask: mask() });
    });
  }
});

// The issue list is paged by a hundred: the header says which range is shown, Prev and Next and the page numbers move, the last page is short, and the
// issues on a page are not on another.
test('SEARCH: a list of 230 issues is shown in pages of 100', async ({ browser }) => {
  test.setTimeout(120_000);
  const ctx = await browser.newContext();
  const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
  expect((await ctx.request.post('/api/v1/login', { headers: H, data: { email: 'admin@example.com', password: 'adminpass1' } })).status()).toBe(200);
  const pk = 'G' + Array.from({ length: 4 }, () => 'ABCDEFGHJKLMNPQRSTUVWXYZ'[Math.floor(Math.random() * 24)]).join('');
  expect((await ctx.request.post('/api/v1/projects', { headers: H, data: { key: pk, name: 'Paging' } })).status()).toBe(201);
  const page = await ctx.newPage();
  await page.goto('/');
  await expect(page.getByRole('button', { name: '+ Create' })).toBeVisible();
  await page.evaluate(async ({ pk }) => {
    const H = { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' };
    for (let b = 0; b < 23; b++) await Promise.all(Array.from({ length: 10 }, (_, i) => fetch('/api/v1/issues', { method: 'POST', headers: H, body: JSON.stringify({ project: pk, type: 'Task', summary: `paged ${b * 10 + i}` }) })));
  }, { pk });
  await page.goto('/#/?syntax=tql&q=' + encodeURIComponent('project = ' + pk));
  const rows = page.getByRole('listitem');
  await expect(rows).toHaveCount(100);
  await expect(page.getByText('230 issues · 1–100')).toBeVisible();
  await expect(page.getByRole('button', { name: '‹ Prev' })).toBeDisabled();
  const first = await rows.allTextContents();
  await page.getByRole('button', { name: '2', exact: true }).click();
  await expect(page.getByText('230 issues · 101–200')).toBeVisible();
  await expect(rows).toHaveCount(100);
  const second = await rows.allTextContents();
  expect(second.filter(t => first.includes(t))).toEqual([]); // nothing on two pages
  await page.getByRole('button', { name: 'Next ›' }).click();
  await expect(page.getByText('230 issues · 201–230')).toBeVisible();
  await expect(rows).toHaveCount(30);
  await expect(page.getByRole('button', { name: 'Next ›' })).toBeDisabled();
  await page.getByRole('button', { name: '‹ Prev' }).click();
  await expect(page.getByText('230 issues · 101–200')).toBeVisible();
});
