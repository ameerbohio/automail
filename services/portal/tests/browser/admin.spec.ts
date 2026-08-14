import { test, expect } from "@playwright/test";
import { submitGuestJob, registerAccount, login, uniqueEmail } from "./helpers";

// Admin flow (roadmap Phase 9 Verify, automated): an operator sees a submitted
// job in /admin/jobs, and a non-admin JWT is refused the ops dashboard.

// Seeded admin (scripts/e2e/seed.sh). Admin is not self-assignable via
// /auth/register, so it is provisioned directly in the database.
const ADMIN_EMAIL = "admin@automail.test";
const ADMIN_PASSWORD = "adminpass123";

test("admin sees a submitted job in the ops dashboard", async ({ page }) => {
  // Create a job so there is something to find, and capture its id.
  const { jobId } = await submitGuestJob(page);

  // Log in as the seeded admin and open the job table. Navigate once: the page
  // loads the job list on mount, and reloading would re-run the session
  // bootstrap (which rotates the refresh token) and race itself into a logout.
  await login(page, ADMIN_EMAIL, ADMIN_PASSWORD);
  await page.goto("/admin/jobs");

  // The table renders the first 8 chars of the id with the full id as the cell
  // title; assert our just-submitted job's row is present.
  await expect(page.locator(`td[title="${jobId}"]`)).toBeVisible({
    timeout: 30_000,
  });
});

test("non-admin sender is refused the ops dashboard", async ({ page }) => {
  // A fresh, ordinary account (role 'sender') has a valid JWT but no admin role.
  await registerAccount(page, uniqueEmail("plainuser"), "password123");

  await page.goto("/admin/jobs");
  // requireAdmin returns 403 -> the page renders the NotAuthorized note.
  await expect(
    page.getByRole("heading", { name: "Not authorized" }),
  ).toBeVisible();
});
