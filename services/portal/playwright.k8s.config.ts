import { defineConfig, devices } from "@playwright/test";

// Browser acceptance for the Kubernetes ingress (Goal K4). Separate from
// playwright.config.ts on purpose: that suite drives the Compose stack on
// plain http://localhost and must keep working untouched (Kubernetes Track
// Process Rule 3). This one drives the real Traefik edge inside the cluster,
// with TLS, the three routed hostnames, the secure-headers CSP and the guest
// rate limit all in the path.
//
// TWO HOST-SPECIFIC FACTS SHAPE THIS FILE, both from plans/16-kubernetes.md §8.1:
//
//  1. THE NAMES DO NOT RESOLVE. /etc/hosts has no *.automail.local entries and
//     adding them needs root, which this host does not grant without a
//     password. Rather than treat the acceptance as blocked, Chromium is given
//     --host-resolver-rules, which is a browser-level equivalent of the hosts
//     file: the page really does request https://automail.local:9443, the
//     browser really does send that Host and SNI, and only the A-record lookup
//     is short-circuited. What is NOT proven this way is the operator step of
//     provisioning DNS — that stays an owner action, recorded as such.
//
//  2. THE EDGE IS ON 9443, NOT 443. Windows holds 443 on this machine. A port
//     is part of an origin, so the CSP connect-src, the MinIO CORS origin and
//     the presigned MINIO_PUBLIC_ENDPOINT all carry :9443 in the k3d overlay.
//     This config is where the browser side of that agreement is asserted.
//
// The edge certificate is self-signed (infra/certs/gen-edge-certs.sh), so
// ignoreHTTPSErrors is on — the same allowance the Compose smoke run makes.
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? "https://automail.local:9443";
const edgeIP = process.env.EDGE_IP ?? "127.0.0.1";

export default defineConfig({
  testDir: "./e2e-k8s",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 30_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    ignoreHTTPSErrors: true,
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        launchOptions: {
          args: [
            // MAP <host> <ip> for each routed name. The port is deliberately
            // absent: the rule maps the name only, so the request keeps its
            // :9443 and the edge sees the origin the CSP was written for.
            `--host-resolver-rules=MAP automail.local ${edgeIP}, MAP api.automail.local ${edgeIP}, MAP blob.automail.local ${edgeIP}`,
          ],
        },
      },
    },
  ],
});
