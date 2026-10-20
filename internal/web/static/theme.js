// Runs in <head>, before the first paint, so the stored theme and the person's display settings (density, font size, contrast,
// motion) are applied without a flash of the defaults. The server holds the settings; this is the copy the last page load left.
(function () {
  var r = document.documentElement;
  try {
    var t = localStorage.getItem('tl-theme');
    if (t === 'light' || t === 'dark') r.dataset.theme = t;
    var p = JSON.parse(localStorage.getItem('tl-ui') || 'null');
    if (p) {
      if (p.theme === 'light' || p.theme === 'dark') r.dataset.theme = p.theme;
      if (/^(compact|default|comfortable)$/.test(p.density)) r.dataset.density = p.density;
      if ([90, 100, 115, 130].indexOf(p.font_scale) >= 0) r.dataset.fs = String(p.font_scale);
      if (p.high_contrast) r.dataset.hc = '';
      if (/^(system|on|off)$/.test(p.reduced_motion)) r.dataset.motion = p.reduced_motion;
    }
  } catch (e) { /* storage unavailable: keep the defaults, dark */ }
})();
