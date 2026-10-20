// Onboarding hints (SPEC 4.6): a first-time person sees one tooltip each for "c", the view buttons and Settings, at most one per
// session. A hint counts as seen once shown, in the person's own settings, so it is never shown again on any device. Hints never
// cover a dialog and never take focus; the "c" hint is not shown when single-key shortcuts are off or on a touch device.
(function () {
  'use strict';
  const A = window.TLApp;
  if (!A) return;
  const { h, S, saveSettings } = A;

  const HINTS = [
    { name: 'create', anchor: () => document.querySelector('.topplus button'), text: 'Press c to create an issue from anywhere. ? shows every shortcut.',
      ok: () => S.settings.shortcuts !== false && !(window.matchMedia && window.matchMedia('(hover: none)').matches) },
    { name: 'views', anchor: () => document.querySelector('.viewctl'), text: 'Short or long rows, the page width and the density are here.', ok: () => true },
    { name: 'settings', anchor: () => document.getElementById('settings-btn'), text: 'Your settings: theme, font size, columns, sort and shortcuts.', ok: () => true },
  ];
  const sessionKey = 'tl-hint-shown';
  const shownThisSession = () => { try { return sessionStorage.getItem(sessionKey) === '1'; } catch (e) { return false; } };
  let current = null;

  function hide() { if (current) { current.remove(); current = null; } }
  function show() {
    if (!S.me || current || shownThisSession() || document.querySelector('.scrim')) return;
    const seen = (S.settings && S.settings.hints_seen) || [];
    const hint = HINTS.find(x => !seen.includes(x.name) && x.ok() && x.anchor() && x.anchor().getClientRects().length);
    if (!hint) return;
    const a = hint.anchor().getBoundingClientRect();
    const tip = h('div', { class: 'hint', role: 'note', 'aria-live': 'polite', 'data-hint': hint.name },
      h('span', {}, hint.text), h('button', { class: 'btn ghost', onclick: hide }, 'Got it'));
    document.body.append(tip);
    const w = tip.offsetWidth;
    tip.style.top = Math.round(a.bottom + 8) + 'px';
    tip.style.left = Math.round(Math.max(8, Math.min(window.innerWidth - w - 8, a.left + a.width / 2 - w / 2))) + 'px';
    current = tip;
    try { sessionStorage.setItem(sessionKey, '1'); } catch (e) { /* storage unavailable: one hint this page load */ }
    saveSettings({ hints_seen: seen.concat(hint.name) }); // seen once shown: never again, on any device
  }
  // a dialog opening takes the hint away: it never covers one
  new MutationObserver(() => { if (current && document.querySelector('.scrim')) hide(); }).observe(document.body, { childList: true });
  const later = () => { hide(); setTimeout(show, 900); };
  window.addEventListener('hashchange', later);
  window.addEventListener('tl-signed-in', later);
  setTimeout(show, 1500);
  A.hints = { show, hide, list: HINTS };
})();
