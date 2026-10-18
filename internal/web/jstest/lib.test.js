const test = require('node:test'), assert = require('node:assert');
const L = require('../static/lib.js');

test('blockDiff marks additions and removals', () => {
  const d = L.blockDiff('a\n\nb\n\nc', 'a\n\nB\n\nc\n\nd');
  assert.deepEqual(d.map(x => x.t + ':' + x.s), ['same:a', 'del:b', 'add:B', 'same:c', 'add:d']);
  assert.deepEqual(L.blockDiff('', 'x'), [{ t: 'add', s: 'x' }]);
  assert.deepEqual(L.blockDiff('x', 'x'), [{ t: 'same', s: 'x' }]);
});

test('applyChoices swaps only chosen conflicts, later ones first', () => {
  const merged = 'a\n\nM1\n\nb\n\nM2a\n\nM2b';
  const conflicts = [
    { block: 1, mine: 'M1', theirs: 'T1' },
    { block: 3, mine: 'M2a\n\nM2b', theirs: 'T2' },
  ];
  assert.equal(L.applyChoices(merged, conflicts, ['mine', 'mine']), merged);
  assert.equal(L.applyChoices(merged, conflicts, ['theirs', 'mine']), 'a\n\nT1\n\nb\n\nM2a\n\nM2b');
  assert.equal(L.applyChoices(merged, conflicts, ['mine', 'theirs']), 'a\n\nM1\n\nb\n\nT2');
  assert.equal(L.applyChoices(merged, conflicts, ['theirs', 'theirs']), 'a\n\nT1\n\nb\n\nT2');
});

test('applyChoices handles a conflict where theirs deleted the block', () => {
  assert.equal(L.applyChoices('a\n\nM', [{ block: 1, mine: 'M', theirs: '' }], ['theirs']), 'a');
});

test('relTime', () => {
  const now = Date.parse('2026-10-19T10:00:00Z');
  assert.equal(L.relTime('2026-10-19T09:59:50Z', now), 'just now');
  assert.equal(L.relTime('2026-10-19T09:58:00Z', now), '2 min ago');
  assert.equal(L.relTime('2026-10-19T07:00:00Z', now), '3 h ago');
  assert.equal(L.relTime('2026-10-17T10:00:00Z', now), '2 d ago');
});

test('inline linkifies only safe schemes', () => {
  const parts = L.inline('see https://a.example/x?y=1 and javascript:alert(1) and mailto:a@b.c.');
  assert.deepEqual(parts.filter(p => p.href).map(p => p.href), ['https://a.example/x?y=1', 'mailto:a@b.c.']);
  assert.ok(!parts.some(p => p.href && p.href.startsWith('javascript')));
});

test('bytes', () => { assert.equal(L.bytes(512), '512 B'); assert.equal(L.bytes(5 << 20), '5.0 MB'); });
