import { test, expect, type Request } from "@playwright/test";
import {
  makePdf,
  PLAINTEXT_MARKER,
  RECIPIENT_QUERY,
  RECIPIENT_MASKED,
} from "../browser/helpers";

// Goal K4: the guest flow in a REAL BROWSER through the cluster's Traefik
// ingress — the half of the acceptance `curl` cannot cover, because a CSP
// violation and a blocked cross-origin PUT exist only in a browser.
//
// Why this cannot reuse e2e/guest.spec.ts: that suite drives the Compose stack
// on http://localhost:3000, where the presigned upload URL points at
// localhost:9000 (its helper matches PUT requests on `:9000/`). Through the
// ingress the same PUT is cross-origin to https://blob.automail.local:9443,
// under a CSP, with a CORS preflight. That difference IS the thing under test.
test("guest flow through the ingress: cross-origin ciphertext PUT, no CSP violations", async ({
  page,
}) => {
  // A CSP violation is reported to the console, never to the server. The §8.1
  // port cascade fails exactly here: a connect-src whose host-source omits
  // :9443 blocks the ciphertext PUT with a console message and no server-side
  // error anywhere. So the console is the evidence.
  const cspViolations: string[] = [];
  page.on("console", (msg) => {
    const text = msg.text();
    if (/Content Security Policy|Refused to (connect|load|execute)/i.test(text)) {
      cspViolations.push(text);
    }
  });
  const pageErrors: string[] = [];
  page.on("pageerror", (err) => pageErrors.push(err.message));

  // --- the portal origin --------------------------------------------------
  await page.goto("/");
  expect(page.url()).toContain(
    process.env.PLAYWRIGHT_BASE_URL ?? "https://automail.local:9443",
  );

  await page.getByPlaceholder("Name or building address").fill(RECIPIENT_QUERY);
  await page.getByRole("button", { name: "Search" }).click();
  // Reaching a result exercises the api path too: the portal's /api proxy ran
  // server-side against the cloud-server Service and the row came from the
  // cluster's Postgres.
  await expect(page.getByText(RECIPIENT_MASKED)).toBeVisible();
  await page.locator('input[name="recipient"]').first().check();

  await page.locator('input[type="file"]').setInputFiles({
    name: "letter.pdf",
    mimeType: "application/pdf",
    buffer: makePdf(),
  });

  // --- the blob origin, cross-origin from the page ------------------------
  const uploadReqPromise: Promise<Request> = page.waitForRequest(
    (req) => req.method() === "PUT" && req.url().includes("blob.automail.local"),
  );
  await page.getByRole("button", { name: "Encrypt & send" }).click();
  const uploadReq = await uploadReqPromise;

  // The same zero-knowledge assertion the Compose suite makes, re-made because
  // the bytes now cross a different origin through a different proxy: what
  // leaves the browser must be ciphertext, never the plaintext PDF.
  const body = uploadReq.postDataBuffer();
  expect(body, "upload body should have been captured").not.toBeNull();
  const bytes = body as Buffer;
  expect(bytes.length).toBeGreaterThan(0);
  expect(bytes.includes(Buffer.from(PLAINTEXT_MARKER, "latin1"))).toBe(false);
  expect(bytes.subarray(0, 5).toString("latin1")).not.toBe("%PDF-");

  // The presigned URL must carry the edge's non-default port (§8.1) — signed
  // by cloud-server from MINIO_PUBLIC_ENDPOINT. Overridable so this same spec
  // can be pointed at a standard-port edge when comparing deploy targets.
  const expectUploadPort = process.env.EXPECT_UPLOAD_PORT ?? ":9443";
  if (expectUploadPort) expect(uploadReq.url()).toContain(expectUploadPort);

  // The upload must have SUCCEEDED, not merely been attempted: a CORS failure
  // or a bad signature would still produce the request above. This is the
  // assertion that proves MINIO_CORS_ORIGIN, the CSP connect-src and the
  // signed endpoint all agree on the same origin *including its port*.
  const uploadResp = await uploadReq.response();
  expect(uploadResp?.status(), "presigned PUT through the blob ingress").toBe(200);

  // Submission completes through the edge and the one-time token is issued.
  await expect(page.getByText("Save this guest token.")).toBeVisible();
  const trackHref = await page
    .getByRole("link", { name: /Track this job/ })
    .getAttribute("href");
  expect(trackHref).toBeTruthy();
  const params = new URLSearchParams((trackHref as string).split("?")[1]);
  expect(params.get("job")).not.toBe("");
  expect(params.get("token")).not.toBe("");

  expect(cspViolations, "CSP violations in the console").toEqual([]);
  expect(pageErrors, "uncaught page errors").toEqual([]);
});

// KNOWN GAP, deliberately visible rather than deleted.
//
// The last clause of the K4 acceptance — watching status transitions live on
// /track — does not pass through ANY Traefik edge, and the failure is not
// Kubernetes-specific:
//
//   browser → portal directly (Compose, http://localhost:3000)  SSE renders ✅
//   browser → Traefik edge (Compose, https://automail.local)    SSE never renders ❌
//   browser → Traefik edge (k3d,     https://automail.local:9443) ❌
//
// It is not the manifests and not the middlewares: cloud-server's own SSE
// streams fine through the same k3d edge, the EventSource request does reach
// cloud-server (a `job:<id>:status` Redis subscription stays live for the
// duration), and removing the rate-limit middleware changes nothing. What
// stalls is the portal's Next.js pass-through relay once a reverse proxy sits
// in front of it. Every existing suite misses this because T7/T8 publish the
// portal's port directly and the deploy smoke uses curl, so no test has ever
// watched SSE through Traefik.
//
// Fixing it means changing portal or edge behaviour, which is an owner
// decision, not something a manifest goal should slip in — logged in
// docs/study/00-interview-pending-questions.md. Marked fixme so the runner
// reports it every time instead of it quietly disappearing.
test.fixme(
  "guest tracking renders live status over SSE through the ingress",
  async () => {
    // Intentionally empty: see the note above. Re-enable when the SSE relay
    // through the edge is resolved; the assertion is `.status strong` reaching
    // a real status on /track?job=…&token=….
  },
);
