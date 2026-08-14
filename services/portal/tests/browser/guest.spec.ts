import { test, expect } from "@playwright/test";
import { submitGuestJob, makePdf, PLAINTEXT_MARKER } from "./helpers";

// Guest flow (roadmap Phase 7 Verify, automated): a sender with no account
// searches a recipient, encrypts a PDF in the browser, uploads the ciphertext,
// submits, receives a one-time guest token, and watches the job climb to
// "delivered" over SSE on /track.
test("guest submits an encrypted job and tracks it to delivered", async ({
  page,
}) => {
  const { jobId, guestToken, uploadBody } = await submitGuestJob(page);

  // Zero-knowledge on the wire: the bytes PUT to object storage must be
  // ciphertext, never the plaintext PDF. This is the client-side proof of the
  // whole project's claim -- "the server only ever stores ciphertext".
  expect(uploadBody, "upload body should have been captured").not.toBeNull();
  const body = uploadBody as Buffer;
  const plaintext = makePdf();
  expect(body.length).toBeGreaterThan(0);
  // AES-256-GCM output = 12-byte IV || ciphertext || 16-byte tag, so it must
  // differ from and not contain the plaintext, and must not carry the PDF magic.
  expect(body.includes(Buffer.from(PLAINTEXT_MARKER, "latin1"))).toBe(false);
  expect(body.subarray(0, 5).toString("latin1")).not.toBe("%PDF-");
  expect(body.equals(plaintext)).toBe(false);

  // Track the job. The confirmation screen linked to /track?job=..&token=..;
  // navigate there and drive the guest-token SSE stream to a terminal state.
  await page.goto(
    `/track?job=${encodeURIComponent(jobId)}&token=${encodeURIComponent(guestToken)}`,
  );
  await page.getByRole("button", { name: /Track|Reconnect/ }).click();

  await expect(page.getByText("Current status:")).toBeVisible();
  await expect(page.locator(".status strong")).toHaveText("delivered", {
    timeout: 60_000,
  });
});
