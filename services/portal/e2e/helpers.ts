import { expect, type Page, type Request } from "@playwright/test";

// A unique, in-the-clear marker embedded in every test PDF. The zero-knowledge
// assertion checks this string never appears in the bytes uploaded to object
// storage -- if it did, the plaintext escaped the browser.
export const PLAINTEXT_MARKER = "AUTOMAIL_PLAINTEXT_SECRET_DO_NOT_LEAK";

// The seeded recipient (scripts/e2e/seed.sh). maskName renders the full name
// "Rivka Testmann" as "R. Testmann" in search results.
export const RECIPIENT_QUERY = "Testmann";
export const RECIPIENT_MASKED = "R. Testmann";

// makePdf builds a minimal, structurally-valid uncompressed PDF containing the
// plaintext marker on its page, so estimatePageCount sees one "/Type /Page"
// and the marker is genuinely in the plaintext we then encrypt.
export function makePdf(marker = PLAINTEXT_MARKER): Buffer {
  const body = [
    "%PDF-1.4",
    "1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj",
    "2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj",
    "3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >> endobj",
    `4 0 obj << /Length 60 >> stream`,
    `BT /F1 12 Tf 20 100 Td (${marker}) Tj ET`,
    "endstream endobj",
    "trailer << /Root 1 0 R >>",
    "%%EOF",
  ].join("\n");
  return Buffer.from(body, "latin1");
}

// uniqueEmail returns a fresh sender email so re-runs never collide with a
// previously-registered account (open self-service registration, 409 on dup).
export function uniqueEmail(prefix = "user"): string {
  return `${prefix}-${Date.now()}-${Math.floor(Math.random() * 1e6)}@automail.test`;
}

export interface SubmittedJob {
  jobId: string;
  guestToken: string;
  uploadBody: Buffer | null;
}

// submitGuestJob drives the full guest submission in the browser: search ->
// select recipient -> choose the PDF -> encrypt in-page -> upload -> submit.
// It captures the raw bytes PUT to object storage so callers can assert they
// are ciphertext. Returns once the "Job submitted" screen is shown.
export async function submitGuestJob(page: Page): Promise<SubmittedJob> {
  await page.goto("/");

  await page.getByPlaceholder("Name or building address").fill(RECIPIENT_QUERY);
  await page.getByRole("button", { name: "Search" }).click();

  // Select the seeded recipient from the results.
  await expect(page.getByText(RECIPIENT_MASKED)).toBeVisible();
  await page.locator('input[name="recipient"]').first().check();

  // Attach the PDF fixture (the browser enforces type === application/pdf).
  await page.locator('input[type="file"]').setInputFiles({
    name: "letter.pdf",
    mimeType: "application/pdf",
    buffer: makePdf(),
  });

  // Capture the direct-to-object-storage PUT (the only request that ever
  // leaves the same-origin /api proxy) so the caller can inspect its body.
  const uploadReqPromise: Promise<Request> = page.waitForRequest(
    (req) => req.method() === "PUT" && /:9000\//.test(req.url()),
  );

  await page.getByRole("button", { name: "Encrypt & send" }).click();

  const uploadReq = await uploadReqPromise;
  const uploadBody = uploadReq.postDataBuffer();

  // The one-time guest token + job id are on the confirmation screen; the
  // "Track this job" link carries both in its query string.
  await expect(page.getByText("Save this guest token.")).toBeVisible();
  const trackHref = await page
    .getByRole("link", { name: /Track this job/ })
    .getAttribute("href");
  if (!trackHref) throw new Error("track link missing on confirmation screen");
  const params = new URLSearchParams(trackHref.split("?")[1]);
  const jobId = params.get("job") ?? "";
  const guestToken = params.get("token") ?? "";
  expect(jobId).not.toBe("");
  expect(guestToken).not.toBe("");

  return { jobId, guestToken, uploadBody };
}

// submitAuthedJob drives the same submission flow as submitGuestJob but for a
// logged-in sender: no guest token is issued and the app redirects straight to
// /jobs/:id. Returns the job id parsed from that URL. Assumes `page` is already
// authenticated.
export async function submitAuthedJob(page: Page): Promise<string> {
  await page.goto("/");
  await page.getByPlaceholder("Name or building address").fill(RECIPIENT_QUERY);
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText(RECIPIENT_MASKED)).toBeVisible();
  await page.locator('input[name="recipient"]').first().check();
  await page.locator('input[type="file"]').setInputFiles({
    name: "letter.pdf",
    mimeType: "application/pdf",
    buffer: makePdf(),
  });
  await page.getByRole("button", { name: "Encrypt & send" }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]+$/, { timeout: 30_000 });
  return page.url().split("/jobs/")[1];
}

// registerAccount registers a fresh sender through the UI; registration
// auto-logs-in and lands on /history.
export async function registerAccount(
  page: Page,
  email: string,
  password = "password123",
): Promise<void> {
  await page.goto("/register");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page).toHaveURL(/\/history$/);
}

// login logs an existing account in through the UI and waits for the
// post-login redirect (away from /login) so the session is settled before the
// caller navigates on.
export async function login(
  page: Page,
  email: string,
  password: string,
): Promise<void> {
  await page.goto("/login");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Log in" }).click();
  await page.waitForURL((url) => !url.pathname.startsWith("/login"));
}
