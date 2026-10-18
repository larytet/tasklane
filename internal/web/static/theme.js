// Runs in <head>, before the first paint, so the stored theme is applied without a flash of the wrong one.
(function () {
  try {
    var t = localStorage.getItem('tl-theme');
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  } catch (e) { /* storage unavailable: keep the default, dark */ }
})();
