import { defineConfig, devices } from '@playwright/test';

/**
 * E2E config for the embedded SPA. Tests assume the orchestrator
 * binary is already running and serving the SPA at PLAYWRIGHT_BASE_URL
 * (default http://127.0.0.1:8080). The CI job in .github/workflows/ci.yml
 * builds + starts the binary before invoking `npx playwright test`.
 *
 * Local dev: `make build-full && ./bin/orchestrator -listen :8080 &`
 * then `cd mobile && npx playwright test`.
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: false, // single orchestrator, sequential tests
  reporter: process.env.CI ? [['github'], ['list']] : 'list',
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? 'http://127.0.0.1:8080',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },
  // Chromium-only is intentional: extra browsers add test time without
  // catching real regressions for a tiny web SPA. Add firefox/webkit
  // here if a future bug surfaces that's browser-engine specific.
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
  timeout: 30_000,
  expect: { timeout: 10_000 },
});
