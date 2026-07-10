"use client";

import { useAdminData } from "@/lib/admin";
import { NotAuthorized, StatusBadge } from "../ui";

// /admin/mailboxes (plans/07): one card per mailbox unit -- status badge, last
// heartbeat, and a plain "Slot 1: 2/5" occupancy list. Polls every 15s. Status
// and occupancy are derived server-side from the Redis printer-state cache; a
// unit with no live cache entry reads "offline" with 0 current occupancy.
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
      <h1>Mailboxes</h1>
      {error && <p className="error">{error}</p>}
      {loading && data === null && <p className="muted">Loading…</p>}
      {data && data.mailboxes.length === 0 && (
        <p className="muted">No mailboxes registered.</p>
      )}

      {data?.mailboxes.map((m) => {
        // Sort slots by number for a stable "Slot 1, 2, 3" reading order.
        const slots = Object.values(m.slot_occupancy).sort(
          (a, b) => a.slot_number - b.slot_number,
        );
        return (
          <section key={m.mailbox_id} className="mailbox-card">
            <div className="mailbox-head">
              <strong>{m.building_address}</strong>
              <StatusBadge status={m.status} />
            </div>
            <p className="muted">
              Last heartbeat:{" "}
              {m.last_heartbeat_at
                ? new Date(m.last_heartbeat_at).toLocaleString()
                : "—"}
            </p>
            {slots.length === 0 ? (
              <p className="muted">No slots configured.</p>
            ) : (
              <ul className="slots">
                {slots.map((s) => (
                  <li key={s.slot_number}>
                    Slot {s.slot_number}: {s.current}/{s.max}
                  </li>
                ))}
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
    </main>
  );
}
