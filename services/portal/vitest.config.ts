import { defineConfig } from "vitest/config";

// Default unit-test config (Goals T4/T6). Cross-language contract tests are
// opt-in — they depend on generated vectors and run via `make crypto-contract`
// (vitest.contract.config.ts), so exclude them from the normal unit run.
export default defineConfig({
  test: {
    include: ["**/*.test.ts"],
    exclude: ["node_modules/**", ".next/**", "**/*.contract.test.ts"],
    passWithNoTests: true,
    coverage: {
      provider: "v8",
      // The logic layer. React UI (app/**/*.tsx, lib/auth.tsx) is exercised by
      // the Playwright E2E in Goal T7, not by these unit tests.
      include: ["lib/**"],
      exclude: ["**/*.test.ts", "**/*.contract.test.ts", "lib/auth.tsx"],
      reporter: ["text-summary", "json-summary"],
      reportsDirectory: "./coverage",
    },
  },
});
