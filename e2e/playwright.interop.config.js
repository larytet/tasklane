import { defineConfig } from '@playwright/test';

// The real-service interoperability scenario (TESTPLAN 3.3): two browser contexts on two different nodes of the compose `interop` profile.
// Started by `make interop`, which sets INTEROP_APP1 and INTEROP_APP2; no server is started here and nothing is faked.
export default defineConfig({
  testDir: './interop',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  use: { channel: 'chrome', headless: true, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
});
