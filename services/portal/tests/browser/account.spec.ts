import { test, expect } from "@playwright/test";
import {
  registerAccount,
  login,
  submitAuthedJob,
  submitGuestJob,
  uniqueEmail,
} from "./helpers";

// Account flow (roadmap Phase 8 Verify, automated): register, submit a job
// while logged in, see it in /history; then a guest submission must NOT appear
// in any account's history.
test("account job appears in history; guest job does not", async ({ page }) => {
  const email = uniqueEmail("account");
  const password = "password123";

  // Register (auto-logs-in) and submit one job as the authenticated sender.
  await registerAccount(page, email, password);
  const jobId = await submitAuthedJob(page);

  // The authenticated /jobs/:id view streams status; wait for delivery.
  await expect(page.locator(".status strong")).toHaveText("delivered", {
    timeout: 60_000,
  });

  // History shows exactly this one job.
  await page.goto("/history");
  const rows = page.locator("table.history tbody tr");
  await expect(rows).toHaveCount(1);
  await expect(page.locator(`a[href="/jobs/${jobId}"]`)).toBeVisible();

  // Log out and submit a job as a guest -- it has no sender_id. (After logout
  // the /history page's own auth-guard redirect can win the race to /login;
  // either way we're logged out -- assert that via the nav, not a specific URL.)
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page.getByRole("link", { name: "Log in" })).toBeVisible();
  await submitGuestJob(page);

  // Log back into the same account: the guest job must be absent, so history
  // still shows only the single account-owned job.
  await login(page, email, password);
  await expect(page).toHaveURL(/\/history$/);
  await expect(page.locator("table.history tbody tr")).toHaveCount(1);
});
