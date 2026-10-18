// Pure helpers shared by the app and its unit tests (no DOM access).
(function (root) {
  'use strict';

  function blocks(s) { return s === '' ? [] : s.split('\n\n'); }

  // LCS-based block diff: returns [{t:'same'|'add'|'del', s}] turning a into b.
  function blockDiff(a, b) {
    const x = blocks(a), y = blocks(b), n = x.length, m = y.length;
    const t = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
    for (let i = n - 1; i >= 0; i--)
      for (let j = m - 1; j >= 0; j--)
        t[i][j] = x[i] === y[j] ? t[i + 1][j + 1] + 1 : Math.max(t[i + 1][j], t[i][j + 1]);
    const out = []; let i = 0, j = 0;
    while (i < n && j < m) {
      if (x[i] === y[j]) { out.push({ t: 'same', s: x[i] }); i++; j++; }
      else if (t[i + 1][j] >= t[i][j + 1]) out.push({ t: 'del', s: x[i++] });
      else out.push({ t: 'add', s: y[j++] });
    }
    while (i < n) out.push({ t: 'del', s: x[i++] });
    while (j < m) out.push({ t: 'add', s: y[j++] });
    return out;
  }

  // The server's merge defaults every conflicting block to "mine". choices[k] is 'mine' | 'theirs' for
  // conflict k; the merged blocks are patched from the last conflict backwards so indices stay valid.
  function applyChoices(merged, conflicts, choices) {
    const bl = blocks(merged);
    const order = conflicts.map((c, k) => k).sort((p, q) => conflicts[q].block - conflicts[p].block);
    for (const k of order) {
      if (choices[k] !== 'theirs') continue;
      const c = conflicts[k];
      const n = blocks(c.mine).length;
      bl.splice(c.block, n, ...blocks(c.theirs));
    }
    return bl.join('\n\n');
  }

  function relTime(iso, now) {
    const s = Math.max(0, Math.round(((now || Date.now()) - new Date(iso).getTime()) / 1000));
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  }

  function uuid() {
    if (root.crypto && root.crypto.randomUUID) return root.crypto.randomUUID();
    return 'id-' + Date.now().toString(36) + Math.random().toString(36).slice(2);
  }

  function bytes(n) {
    if (n < 1024) return n + ' B';
    if (n < 1 << 20) return (n / 1024).toFixed(0) + ' KB';
    if (n < 1 << 30) return (n / (1 << 20)).toFixed(1) + ' MB';
    return (n / (1 << 30)).toFixed(1) + ' GB';
  }

  // Split text into paragraphs and linkify only http, https and mailto URLs; returns plain descriptors so
  // the caller builds DOM nodes (markup is never built from strings).
  function inline(s) {
    const out = []; const re = /\b(https?:\/\/[^\s<>"')]+|mailto:[^\s<>"')]+)/g; let last = 0, m;
    while ((m = re.exec(s))) {
      if (m.index > last) out.push({ text: s.slice(last, m.index) });
      out.push({ text: m[0], href: m[0] }); last = m.index + m[0].length;
    }
    if (last < s.length) out.push({ text: s.slice(last) });
    return out;
  }

  const api = { blocks, blockDiff, applyChoices, relTime, uuid, bytes, inline };
  if (typeof module !== 'undefined') module.exports = api; else root.TL = api;
})(typeof window !== 'undefined' ? window : globalThis);
