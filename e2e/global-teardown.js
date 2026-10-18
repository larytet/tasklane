export default async function globalTeardown() {
  const child = globalThis.__tasklane;
  if (child) child.kill('SIGTERM');
  else if (process.env.E2E_PID) { try { process.kill(Number(process.env.E2E_PID), 'SIGTERM'); } catch (e) { /* already gone */ } }
}
