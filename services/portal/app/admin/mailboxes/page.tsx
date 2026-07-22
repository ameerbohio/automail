"use client";

import { useAdminData } from "@/lib/admin";
import { NotAuthorized, StatusBadge } from "../ui";

// /admin/mailboxes (plans/07): one card per mailbox unit -- status badge, last
// heartbeat, and slot occupancy. Polls every 15s. Status and occupancy are
// derived server-side from the Redis printer-state cache; a unit with no live
// cache entry reads "offline" with 0 current occupancy.
const POLL_MS = 15_000;

interface SlotOccupancy {
  slot_number: number;
  current: number;
  max: number;
}

interface MailboxRow {
  mailbox_id: string;
  building_address: string;
  status: string;
  last_heartbeat_at?: string;
  slot_occupancy: Record<string, SlotOccupancy>;
}

interface Mailboxes {
  mailboxes: MailboxRow[];
}

export default function AdminMailboxes() {
  const { data, error, forbidden, loading } = useAdminData<Mailboxes>(
    "/api/admin/mailboxes",
    POLL_MS,
  );

  if (forbidden) return <NotAuthorized />;

  return (
    <main>
      <div className="page-head">
        <h1>Mailboxes</h1>
        <p className="muted">Refreshes every 15s</p>
      </div>

      {error && <p className="callout">{error}</p>}
      {loading && data === null && (
        <div className="skeleton-stack" aria-hidden="true">
          <div className="skeleton" />
          <div className="skeleton" style={{ width: "60%" }} />
        </div>
      )}
      {data && data.mailboxes.length === 0 && (
        <div className="empty">
          <p>No mailboxes registered.</p>
        </div>
      )}

      <div className="mailbox-grid">
        {data?.mailboxes.map((m, i) => {
          // Sort slots by number for a stable "Slot 1, 2, 3" reading order.
          const slots = Object.values(m.slot_occupancy).sort(
            (a, b) => a.slot_number - b.slot_number,
          );
          return (
            <section
              key={m.mailbox_id}
              className="mailbox-card"
              style={{ "--i": i } as React.CSSProperties}
            >
              <div className="mailbox-head">
                <strong>{m.building_address}</strong>
                <StatusBadge status={m.status} />
              </div>
              <p className="muted" style={{ fontSize: "0.8125rem" }}>
                Last heartbeat:{" "}
                {m.last_heartbeat_at
                  ? new Date(m.last_heartbeat_at).toLocaleString()
                  : "—"}
              </p>

              {slots.length === 0 ? (
                <p className="muted">No slots configured.</p>
              ) : (
                <ul className="slots">
                  {slots.map((s) => {
                    const pct = s.max > 0 ? (s.current / s.max) * 100 : 0;
                    return (
                      <li key={s.slot_number}>
                        <span>Slot {s.slot_number}</span>
                        {/* Occupancy as a bar as well as a number: a full slot
                            is the thing an operator needs to spot at a glance. */}
                        <span
                          className={`meter${pct >= 100 ? " is-full" : ""}`}
                          style={{ "--fill": `${pct}%` } as React.CSSProperties}
                        >
                          <span />
                        </span>
                        <span className="slot-count">
                          {s.current}/{s.max}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              )}

              {/* Consumable alert stub (plans/07): ink % and paper count are not
                  wired to hardware in the prototype -- placeholder fields only. */}
              <p className="muted consumables">
                Consumables (stub, not wired to hardware): Ink —, Paper —
              </p>
            </section>
          );
        })}
      </div>
    </main>
  );
}
