import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { pageErrors, newUser, login, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];
async function axe(page, what) {
  const r = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const bad = r.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
  expect(bad.map(v => v.id + ': ' + v.nodes.slice(0, 2).map(n => n.target.join(' ')).join(', ')), what).toEqual([]);
}
const DEFAULTS = { theme: '', density: 'default', list_view: 'short', page_width: 'wide', snippet_chars: 500, font_scale: 100, reduced_motion: 'system', high_contrast: false, shortcuts: true };
const put = (page, data) => page.evaluate(async d => (await fetch('/api/v1/me/ui-prefs', { method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane' }, body: JSON.stringify(d) })).status, data);
const get = page => page.evaluate(async () => (await fetch('/api/v1/me/ui-prefs', { credentials: 'same-origin' })).json());
const root = (page, k) => page.evaluate(k => document.documentElement.dataset[k] ?? null, k);

for (const theme of ['dark', 'light']) {
  test(`PREF-05: the Settings dialog is a modal operable by keyboard and applies changes at once (${theme})`, async ({ browser }) => {
    const u = await newUser(browser, 'arkady@example.com');
    await u.page.addInitScript(t => localStorage.setItem('tl-theme', t), theme);
    await u.page.reload();
    const gear = u.page.getByRole('button', { name: 'Settings' });
    await gear.click();
    const dlg = u.page.getByRole('dialog', { name: 'Settings' });
    await expect(dlg).toBeVisible();
    expect(await u.page.evaluate(() => !!document.activeElement.closest('[role=dialog]'))).toBe(true);
    await axe(u.page, 'settings dialog, appearance');
    // the tabs move with the arrow keys
    await dlg.getByRole('tab', { name: 'Appearance' }).focus();
    await u.page.keyboard.press('ArrowRight');
    await expect(dlg.getByRole('tab', { name: 'List' })).toBeFocused();
    await expect(dlg.getByRole('tab', { name: 'List' })).toHaveAttribute('aria-selected', 'true');
    await expect(dlg.getByRole('tabpanel')).toContainText('Density');
    await axe(u.page, 'settings dialog, list');
    await u.page.keyboard.press('End');
    await expect(dlg.getByRole('tab', { name: 'API tokens' })).toBeFocused(); // the last tab: Settings now holds the account's sections too
    await u.page.keyboard.press('Home');
    await expect(dlg.getByRole('tab', { name: 'Appearance' })).toBeFocused();
    // focus stays inside the dialog
    for (let i = 0; i < 25; i++) {
      await u.page.keyboard.press('Tab');
      expect(await u.page.evaluate(() => !!document.activeElement.closest('[role=dialog]')), 'focus left the dialog').toBe(true);
    }
    // a change applies without a reload and is saved on the server
    await dlg.getByLabel('Font size').selectOption('115');
    await expect.poll(() => root(u.page, 'fs')).toBe('115');
    await dlg.getByLabel('High contrast').check();
    await expect.poll(() => root(u.page, 'hc')).toBe('');
    await expect(dlg.getByRole('status')).toHaveText('Saved');
    const saved = await get(u.page);
    expect(saved.prefs.font_scale).toBe(115);
    expect(saved.prefs.high_contrast).toBe(true);
    await axe(u.page, 'settings dialog in high contrast');
    // Escape closes and gives focus back to the button that opened it
    await u.page.keyboard.press('Escape');
    await expect(dlg).toHaveCount(0);
    await expect(gear).toBeFocused();
    await axe(u.page, 'issue list at 115% in high contrast');
    expect(await put(u.page, DEFAULTS)).toBe(200);
    await u.ctx.close();
  });
}

test('PREF-03: settings follow the person to another browser and never reach the next person on this one', async ({ browser }) => {
  const a = await newUser(browser, 'arkady@example.com');
  expect(await put(a.page, { density: 'compact', high_contrast: true, font_scale: 130 })).toBe(200);
  const b = await newUser(browser, 'arkady@example.com'); // a second browser: no local copy, the server's settings
  expect(await root(b.page, 'density')).toBe('compact');
  expect(await root(b.page, 'hc')).toBe('');
  expect(await root(b.page, 'fs')).toBe('130');
  // sign out on the first browser, and somebody else signs in on it
  await a.page.reload();
  expect(await root(a.page, 'fs')).toBe('130');
  await a.page.getByRole('button', { name: 'Sign out' }).click();
  await expect(a.page.getByLabel('Email')).toBeVisible();
  expect(await root(a.page, 'hc')).toBeNull();
  expect(await root(a.page, 'fs')).toBeNull();
  expect(await a.page.evaluate(() => localStorage.getItem('tl-ui'))).toBeNull();
  await login(a.page, 'jana@example.com');
  expect(await root(a.page, 'hc')).toBeNull();
  expect(await root(a.page, 'fs')).toBeNull();
  expect(await root(a.page, 'density')).toBe('default');
  expect(await put(b.page, DEFAULTS)).toBe(200);
  await a.ctx.close(); await b.ctx.close();
});

test('PREF-02: two tabs change different settings and both changes are kept', async ({ browser }) => {
  const u = await newUser(browser, 'arkady@example.com');
  const tab2 = await u.ctx.newPage();
  await tab2.goto('/');
  await expect(tab2.getByRole('button', { name: 'Settings' })).toBeVisible();
  // the first tab saves; the second still holds the older version and saves something else
  await u.page.getByRole('button', { name: 'Settings' }).click();
  await u.page.getByRole('dialog').getByLabel('High contrast').check();
  await expect(u.page.getByRole('dialog').getByRole('status')).toHaveText('Saved');
  await tab2.getByRole('button', { name: 'Settings' }).click();
  await tab2.getByRole('dialog').getByRole('tab', { name: 'List' }).click();
  await tab2.getByRole('dialog').getByRole('radiogroup', { name: 'Density' }).getByLabel('Comfortable', { exact: true }).check();
  await expect.poll(async () => (await get(tab2)).prefs.density).toBe('comfortable');
  const p = (await get(tab2)).prefs;
  expect(p.high_contrast, 'the first tab\'s change was lost').toBe(true);
  expect(await root(tab2, 'hc'), 'the second tab took the merged settings').toBe('');
  expect(await put(tab2, DEFAULTS)).toBe(200);
  await u.ctx.close();
});

test('LIST-01: density is set in Settings (not on the list) and sets the row height (compact 36, default 44, comfortable 56)', async ({ browser }) => {
  const u = await newUser(browser, 'arkady@example.com', { viewport: { width: 1280, height: 800 } });
  const view = u.page.getByRole('group', { name: 'View' });
  await expect(view.getByRole('button', { name: /^Long view/ })).toBeVisible();
  await expect(view.getByRole('button', { name: 'Compact', exact: true })).toHaveCount(0);
  const row = u.page.getByRole('listitem').first();
  await u.page.getByRole('button', { name: 'Settings' }).click();
  const dlg = u.page.getByRole('dialog', { name: 'Settings' });
  await dlg.getByRole('tab', { name: 'List' }).click();
  for (const [name, px] of [['Compact', 36], ['Comfortable', 56], ['Default', 44]]) {
    await dlg.getByRole('radiogroup', { name: 'Density' }).getByLabel(name, { exact: true }).check();
    await expect.poll(async () => Math.round((await row.boundingBox()).height)).toBeGreaterThanOrEqual(px);
    expect(Math.round((await row.boundingBox()).height)).toBeLessThanOrEqual(px + 4);
  }
  await u.page.keyboard.press('Escape');
  // each density works in the long view too, without sideways scroll on a phone
  await view.getByRole('button', { name: /^Long view/ }).click();
  await u.page.setViewportSize({ width: 390, height: 800 });
  for (const d of ['compact', 'default', 'comfortable']) {
    expect(await put(u.page, { density: d })).toBe(200);
    await u.page.reload();
    await expect(u.page.getByRole('listitem').first()).toBeVisible();
    const o = await u.page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(o, `long view, ${d}: sideways scroll at 390 px`).toBeLessThanOrEqual(0);
  }
  expect(await put(u.page, DEFAULTS)).toBe(200);
  await u.ctx.close();
});

test('THEME-01: every font size fits a phone on the list, the issue page and in Settings', async ({ browser }) => {
  const u = await newUser(browser, 'arkady@example.com', { viewport: { width: 390, height: 800 } });
  for (const fs of [90, 100, 115, 130]) {
    expect(await put(u.page, { font_scale: fs })).toBe(200);
    for (const route of ['/', '/#/issue/ABC-1']) {
      await u.page.goto(route);
      await expect(u.page.getByRole('button', { name: 'Settings' })).toBeVisible();
      if (route.includes('issue')) await expect(u.page.getByLabel('Summary')).toBeVisible(); else await expect(u.page.getByRole('listitem').first()).toBeVisible();
      await expect.poll(() => u.page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth), `${route} at ${fs}%: sideways scroll`).toBeLessThanOrEqual(0);
    }
    await u.page.getByRole('button', { name: 'Settings' }).click();
    await expect(u.page.getByRole('dialog')).toBeVisible();
    const dlg = await u.page.getByRole('dialog').boundingBox();
    expect(dlg.x).toBeGreaterThanOrEqual(0);
    await u.page.keyboard.press('Escape');
  }
  expect(await put(u.page, DEFAULTS)).toBe(200);
  await u.ctx.close();
});

test('THEME-02: reduced motion follows the system by default and the setting overrides it both ways', async ({ browser }) => {
  const anim = async page => page.evaluate(() => {
    const t = document.createElement('div'); t.className = 'toast'; document.getElementById('toasts').append(t);
    const f = document.createElement('div'); f.className = 'flash'; document.body.append(f);
    const out = { toast: getComputedStyle(t).animationName, flash: getComputedStyle(f).animationName };
    t.remove(); f.remove(); return out;
  });
  for (const [system, setting, toast, flash] of [
    ['reduce', 'system', 'tl-fade', 'none'], ['no-preference', 'system', 'tl-in', 'flash'],
    ['no-preference', 'on', 'tl-fade', 'none'], ['reduce', 'off', 'tl-in', 'flash']]) {
    const u = await newUser(browser, 'arkady@example.com', { reducedMotion: system });
    expect(await put(u.page, { reduced_motion: setting })).toBe(200);
    await u.page.reload();
    await expect(u.page.getByRole('button', { name: 'Settings' })).toBeVisible();
    expect(await anim(u.page), `system ${system}, setting ${setting}`).toEqual({ toast, flash });
    await u.ctx.close();
  }
  const u = await newUser(browser, 'arkady@example.com');
  expect(await put(u.page, DEFAULTS)).toBe(200);
  await u.ctx.close();
});

test('THEME-04: font size, contrast and density are on the page before the first paint after a reload', async ({ browser }) => {
  const u = await newUser(browser, 'arkady@example.com');
  expect(await put(u.page, { font_scale: 115, high_contrast: true, density: 'comfortable' })).toBe(200);
  await u.page.reload(); // the page learns them from the server and keeps a copy for the next first paint
  await expect(u.page.getByRole('button', { name: 'Settings' })).toBeVisible();
  await u.page.addInitScript(() => {
    window.__atBody = null;
    const obs = new MutationObserver(() => {
      if (document.body && window.__atBody === null) { const d = document.documentElement.dataset; window.__atBody = { fs: d.fs, hc: d.hc, density: d.density }; obs.disconnect(); }
    });
    obs.observe(document, { childList: true, subtree: true });
  });
  await u.page.reload();
  await expect(u.page.getByRole('button', { name: 'Settings' })).toBeVisible();
  expect(await u.page.evaluate(() => window.__atBody)).toEqual({ fs: '115', hc: '', density: 'comfortable' });
  expect(await put(u.page, DEFAULTS)).toBe(200);
  await u.ctx.close();
});
