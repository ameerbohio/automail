// Small presentational pieces shared across the three ops-dashboard pages.

export function StatusBadge({ status }: { status: string }) {
  // Class per status so idle/printing/offline/delivered/failed each get their
  // own color (globals.css). Unknown statuses fall back to the base badge.
  return <span className={`badge badge-${status}`}>{status}</span>;
}

export function NotAuthorized() {
  return (
    <main>
      <h1>Not authorized</h1>
      <p className="muted">
        Your account doesn&rsquo;t have operator access. The ops dashboard is
        restricted to admin accounts.
      </p>
    </main>
  );
}
