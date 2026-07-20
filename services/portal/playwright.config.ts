import { defineConfig, devices } from "@playwright/test";

// Browser E2E for the portal (testing-plan Part 4b / Goal T7). These run
// against the already-running compose stack (see `make test-e2e`), not a
// Next dev server -- the point is to exercise the real assembled product
// (portal -> cloud -> Redis -> printer decrypt -> SSE status) in a browser.
//
// The stack is brought up on plain http://localhost by docker-compose.e2e.yml
// (a browser "secure context", so Web Crypto and Secure cookies still work)
// to avoid the self-signed-TLS / mixed-content friction of the Traefik edge.
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? "http://localhost:3000";

export default defineConfig({
  testDir: "./e2e",
  // Serial: the suites share one seeded database, and history/admin row
  // assertions reason about how many jobs exist, so parallel workers would
  // race. Correctness over speed for a handful of end-to-end journeys.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  // A job has to climb submitted -> ... -> delivered through real dispatch and
  // an mTLS printer round-trip, so give each test generous headroom.
  timeout: 90_000,
  expect: { timeout: 20_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    trace: "on-first-retry",
    // http://localhost only, but harmless if the stack is ever run behind TLS.
    ignoreHTTPSErrors: true,
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
});
