import { defineConfig } from "vitest/config";

// Cross-language crypto contract only (Goal T2 / Part 3). Driven by
// `make crypto-contract`, which runs the Go generate/verify steps around it.
export default defineConfig({
  test: {
    include: ["**/*.contract.test.ts"],
    exclude: ["node_modules/**", ".next/**"],
    // Deterministic order not required, but keep a single file's tests in order.
    fileParallelism: false,
  },
});
