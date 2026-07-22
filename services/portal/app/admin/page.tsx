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

// Each tile carries a colour key on its left edge so the four numbers are
// scannable as a group -- neutral for volume, accent for in-flight, red for
// the one an operator has to act on.
function Stat({
  n,
  value,
  label,
  color,
}: {
  n: number;
  value: number | string;
  label: string;
  color?: string;
}) {
  return (
    <div
      className="stat"
      style={{ "--i": n, "--stat-color": color } as React.CSSProperties}
    >
      <span className="stat-num">{value}</span>
      <span className="stat-label">{label}</span>
    </div>
  );
}

export default function AdminOverview() {
  const summary = useAdminData<Summary>("/api/admin/summary", POLL_MS);
  const mailboxes = useAdminData<Mailboxes>("/api/admin/mailboxes", POLL_MS);

  if (summary.forbidden || mailboxes.forbidden) {
    return <NotAuthorized />;
  }

  const counts = summary.data?.status_counts ?? {};
  const failed = counts["failed"] ?? 0;

  return (
    <main>
      <div className="page-head">
        <h1>Overview</h1>
        <p className="muted">Refreshes every 15s</p>
      </div>
      {summary.error && <p className="callout">{summary.error}</p>}

      <div className="stats">
        <Stat
          n={0}
          value={summary.data?.queue_depth ?? "—"}
          label="In queue"
          color="var(--accent)"
        />
        <Stat
          n={1}
          value={summary.data?.completed_today ?? "—"}
          label="Completed today"
          color="var(--ok)"
        />
        <Stat
          n={2}
          value={counts["printing"] ?? 0}
          label="Printing now"
          color="var(--accent)"
        />
        <Stat
          n={3}
          value={failed}
          label="Failed"
          color={failed > 0 ? "var(--err)" : undefined}
        />
      </div>

      <h2 style={{ marginBottom: "0.75rem" }}>Mailboxes</h2>
      {mailboxes.error && <p className="callout">{mailboxes.error}</p>}
      {mailboxes.data === null && !mailboxes.error && (
        <div className="skeleton-stack" aria-hidden="true">
          <div className="skeleton" />
          <div className="skeleton" style={{ width: "70%" }} />
        </div>
      )}
      {mailboxes.data && mailboxes.data.mailboxes.length === 0 && (
        <p className="muted">No mailboxes registered.</p>
      )}
      {mailboxes.data && mailboxes.data.mailboxes.length > 0 && (
        <div className="table-card">
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
                  <td data-label="Address">{m.building_address}</td>
                  <td data-label="Status">
                    <StatusBadge status={m.status} />
                  </td>
                  <td data-label="Heartbeat">
                    {m.last_heartbeat_at
                      ? new Date(m.last_heartbeat_at).toLocaleString()
                      : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}
