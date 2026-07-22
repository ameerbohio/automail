// Small presentational pieces shared across the three ops-dashboard pages.

import { IconLock } from "../icons";

export function StatusBadge({ status }: { status: string }) {
  // Class per status so idle/printing/offline/delivered/failed each get their
  // own color (globals.css). Unknown statuses fall back to the base badge.
  return <span className={`badge badge-${status}`}>{status}</span>;
}

export function NotAuthorized() {
  return (
    <main>
      <div className="empty">
        <IconLock size={34} />
        <h1>Not authorized</h1>
        <p>
          Your account doesn&rsquo;t have operator access. The ops dashboard is
          restricted to admin accounts.
        </p>
      </div>
    </main>
  );
}
