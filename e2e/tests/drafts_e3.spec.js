import { test, expect } from '@playwright/test';
import { pageErrors, newUser, createIssue, closeContexts } from './helpers.js';

test.beforeEach(() => { pageErrors.length = 0; });
test.afterEach(async () => { const errs = [...pageErrors]; await closeContexts(); expect(errs, 'uncaught errors in the page').toEqual([]); });

// E3 (SPEC 21, "lost draft"): text typed in an open editor can be recovered after a tab close, a crash or a reload, and what is lost is at most the local
// autosave interval (2 s of typing). A simulated person types without stopping at a steady rate, the tab dies at a random moment in one of three ways, and
// a new tab is opened: what the page offers back is compared with what was in the editor at every moment. The loss window is the time between the moment the
// recovered text was typed and the moment of the crash. Typing without a pause is the case that matters: a draft that is only written when typing pauses
// is never written for someone who does not pause.
const SESSIONS = Number(process.env.E3_SESSIONS || 9);
const ALLOWED_MS = 2000 + 1500; // the interval, plus the time a chunk takes to type and a timer to fire

test(`E3: ${SESSIONS} editing sessions typing without pausing, killed at random, lose at most the autosave interval`, async ({ browser }) => {
  test.setTimeout(60_000 + SESSIONS * 20_000);
  const person = await newUser(browser, 'jana@example.com');
  const results = [];
  for (let s = 0; s < SESSIONS; s++) {
    const mode = ['close', 'crash', 'reload'][s % 3];
    const key = await createIssue(person.page, 'E3 session ' + s, 'start');
    const page = await person.ctx.newPage();
    page.on('dialog', d => d.accept().catch(() => {})); // "Leaving with unsaved changes" is answered yes: the person is gone
    await page.goto(`/#/issue/${key}`);
    await page.getByRole('button', { name: 'Edit' }).click();
    const editor = page.getByLabel('Description editor');
    await editor.fill('S');
    const timeline = [{ t: Date.now(), text: 'S' }]; // what the editor held, and since when
    const t0 = Date.now();
    const crashAfter = 2500 + Math.floor(Math.random() * 9000); // between 2.5 and 11.5 s of typing
    let n = 0;
    while (Date.now() - t0 < crashAfter) {
      await editor.pressSequentially('abcde', { delay: 20 }); // five characters, then a short breath: no pause near 2 s, ever
      n++;
      timeline.push({ t: Date.now(), text: await editor.inputValue() });
      await page.waitForTimeout(120);
    }
    const stored = await page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('tl-draft')).map(k => JSON.parse(localStorage[k]).body.length)).catch(() => []);
    const crashAt = Date.now();
    const typedAtCrash = timeline[timeline.length - 1].text;
    if (mode === 'close') await page.close({ runBeforeUnload: false });
    else if (mode === 'crash') { await page.goto('chrome://crash').catch(() => {}); await page.close({ runBeforeUnload: false }).catch(() => {}); }
    else { /* reload: the tab is replaced by itself */ }
    const back = mode === 'reload' ? page : await person.ctx.newPage();
    if (mode === 'reload') await page.reload();
    else await back.goto(`/#/issue/${key}`);
    let recovered = '';
    const banner = back.getByText(/Restored your unsaved draft/);
    if (await banner.first().waitFor({ state: 'visible', timeout: 10000 }).then(() => true, () => false)) { // (isVisible does not wait)
      await back.getByRole('button', { name: 'Continue editing' }).click().catch(() => {});
      recovered = await back.getByLabel('Description editor').inputValue().catch(() => '');
    }
    // the recovered text must be what was typed up to some moment, and that moment must be recent
    expect(typedAtCrash.startsWith(recovered) || recovered === '', `session ${s} (${mode}): the recovered text is not a prefix of what was typed`).toBeTruthy();
    let since = t0;
    for (const e of timeline) if (e.text.length <= recovered.length) since = e.t;
    const window = crashAt - since;
    results.push({ s, mode, typed: typedAtCrash.length, recovered: recovered.length, window, stored: stored.join('/') });
    await back.close().catch(() => {});
  }
  console.log('E3 sessions: ' + results.map(r => `${r.mode}:${r.recovered}/${r.typed} chars (stored ${r.stored}), window ${r.window} ms`).join(' | '));
  const worst = Math.max(...results.map(r => r.window));
  expect(worst, `the longest loss window was ${worst} ms; the allowance is ${ALLOWED_MS} ms (2 s of typing)`).toBeLessThanOrEqual(ALLOWED_MS);
});
