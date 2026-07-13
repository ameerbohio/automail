import { defineConfig } from "vitest/config";

// Default unit-test config (Goals T4/T6). Cross-language contract tests are
// opt-in — they depend on generated vectors and run via `make crypto-contract`
// (vitest.contract.config.ts), so exclude them from the normal unit run.
export default defineConfig({
  test: {
    include: ["**/*.test.ts"],
    exclude: ["node_modules/**", ".next/**", "**/*.contract.test.ts"],
    // Unit tests land in Goals T4/T6; until then `npm test` is a clean no-op.
    passWithNoTests: true,
  },
});
