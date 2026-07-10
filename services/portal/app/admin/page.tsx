"use client";

import { useAdminData } from "@/lib/admin";
import { NotAuthorized, StatusBadge } from "./ui";

// /admin overview (plans/07-ops-dashboard.md): queue depth, jobs completed
// today, and a per-unit mailbox status summary. Polls every 15s -- no SSE here,
// this is coarse operational visibility, not live per-job tracking.
const POLL_MS = 15_000;

interface Summary {
  status_counts: Record<string, number>;
  queue_depth: number;
  completed_today: number;
}

interface MailboxRow {
  mailbox_id: string;
  building_address: string;
  status: string;
  last_heartbeat_at?: string;
}

interface Mailboxes {
  mailboxes: MailboxRow[];
}

export default function AdminOverview() {
  const summary = useAdminData<Summary>("/api/admin/summary", POLL_MS);
  const mailboxes = useAdminData<Mailboxes>("/api/admin/mailboxes", POLL_MS);

  if (summary.forbidden || mailboxes.forbidden) {
    return <NotAuthorized />;
  }

  const counts = summary.data?.status_counts ?? {};

  return (
    <main>
      <h1>Overview</h1>
      {summary.error && <p className="error">{summary.error}</p>}

      <div className="stats">
        <div className="stat">
          <span className="stat-num">{summary.data?.queue_depth ?? "—"}</span>
          <span className="stat-label">In queue</span>
        </div>
        <div className="stat">
          <span className="stat-num">{summary.data?.completed_today ?? "—"}</span>
          <span className="stat-label">Completed today</span>
        </div>
        <div className="stat">
          <span className="stat-num">{counts["printing"] ?? 0}</span>
          <span className="stat-label">Printing now</span>
        </div>
        <div className="stat">
          <span className="stat-num">{counts["failed"] ?? 0}</span>
          <span className="stat-label">Failed</span>
        </div>
      </div>

      <h2>Mailboxes</h2>
      {mailboxes.error && <p className="error">{mailboxes.error}</p>}
      {mailboxes.data === null && !mailboxes.error && (
        <p className="muted">Loading…</p>
      )}
      {mailboxes.data && mailboxes.data.mailboxes.length === 0 && (
        <p className="muted">No mailboxes registered.</p>
      )}
      {mailboxes.data && mailboxes.data.mailboxes.length > 0 && (
        <table className="history">
          <thead>
            <tr>
              <th>Address</th>
              <th>Status</th>
              <th>Last heartbeat</th>
            </tr>
          </thead>
          <tbody>
            {mailboxes.data.mailboxes.map((m) => (
              <tr key={m.mailbox_id}>
                <td>{m.building_address}</td>
                <td>
                  <StatusBadge status={m.status} />
                </td>
                <td>
                  {m.last_heartbeat_at
                    ? new Date(m.last_heartbeat_at).toLocaleString()
                    : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
