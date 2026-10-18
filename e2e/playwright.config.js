import { defineConfig } from '@playwright/test';

// The suite drives the real binary in the system Chrome (no browser download). global-setup starts a fresh server.
export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 8_000, toHaveScreenshot: { maxDiffPixelRatio: 0.01 } },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  globalSetup: './global-setup.js',
  globalTeardown: './global-teardown.js',
  use: {
    channel: 'chrome',
    headless: true,
    baseURL: process.env.E2E_BASE_URL || 'http://127.0.0.1:18181',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
