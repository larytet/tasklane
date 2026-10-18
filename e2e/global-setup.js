import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const PORT = 18181;
const BASE = `http://127.0.0.1:${PORT}`;
const PG = process.env.E2E_PG || 'postgres://tl:tl@localhost:55432';

async function api(cookie, method, path, body) {
  const r = await fetch(BASE + path, { method, headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'tasklane', ...(cookie ? { Cookie: cookie } : {}) }, body: body ? JSON.stringify(body) : undefined });
  const text = await r.text();
  return { status: r.status, cookie: (r.headers.get('set-cookie') || '').split(';')[0], data: text ? JSON.parse(text) : null };
}

export default async function globalSetup() {
  const db = 'tl_e2e';
  execFileSync('psql', [`${PG}/postgres`, '-qc', `DROP DATABASE IF EXISTS ${db} WITH (FORCE)`, '-c', `CREATE DATABASE ${db}`], { stdio: 'inherit' });
  const dir = mkdtempSync(join(tmpdir(), 'tasklane-e2e-'));
  const bin = join(dir, 'tasklane');
  execFileSync('go', ['build', '-o', bin, './cmd/tasklane'], { cwd: '..', stdio: 'inherit', env: { ...process.env, PATH: process.env.PATH + ':/usr/local/go/bin' } });
  const child = spawn(bin, ['serve'], {
    env: { ...process.env, TASKLANE_DATABASE_URL: `${PG}/${db}`, TASKLANE_LISTEN: `127.0.0.1:${PORT}`, TASKLANE_BLOB_DIR: join(dir, 'blobs'), TASKLANE_ADMIN_EMAIL: 'admin@example.com', TASKLANE_ADMIN_PASSWORD: 'adminpass1', TASKLANE_BASE_URL: BASE },
    stdio: ['ignore', 'inherit', 'inherit'],
  });
  writeFileSync(join(dir, 'pid'), String(child.pid));
  process.env.E2E_PID = String(child.pid);
  globalThis.__tasklane = child;
  for (let i = 0; i < 60; i++) {
    try { const r = await fetch(BASE + '/readyz'); if (r.ok) break; } catch (e) { /* not up yet */ }
    await new Promise(r => setTimeout(r, 250));
  }
  const admin = await api(null, 'POST', '/api/v1/login', { email: 'admin@example.com', password: 'adminpass1' });
  const ck = admin.cookie;
  await api(ck, 'POST', '/api/v1/projects', { key: 'ABC', name: 'Alpha' });
  await api(ck, 'POST', '/api/v1/projects', { key: 'XYZ', name: 'Xray' });
  const ids = {};
  for (const [email, name] of [['jana@example.com', 'Jana K.'], ['arkady@example.com', 'Arkady M.'], ['viewer@example.com', 'Vera V.']]) {
    const u = await api(ck, 'POST', '/api/v1/users', { email, name, password: 'password123' });
    ids[email] = u.data.id;
    await api(ck, 'POST', '/api/v1/projects/ABC/members', { user: u.data.id, role: email.startsWith('viewer') ? 'viewer' : 'member' });
    if (!email.startsWith('viewer')) await api(ck, 'POST', '/api/v1/projects/XYZ/members', { user: u.data.id, role: 'member' });
  }
  await api(ck, 'POST', '/api/v1/issues', { project: 'ABC', type: 'Bug', summary: 'Checkout fails on 3DS challenge', priority: 'high', labels: ['payments'],
    description: 'Steps:\n\n1. Use test card\n\nExpected: order created\n\nNotes: see https://example.com/runbook' });
  process.env.E2E_READY = '1';
}
