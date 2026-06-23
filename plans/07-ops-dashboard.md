# Ops Dashboard

**Language**: TypeScript (Next.js, same app as sender portal — separate route group)  
**Tag**: [SIMPLE]  
**Role**: Minimal visibility into system state for a property manager or operator. Proof the system is running.

The ops dashboard does not expose document content. It shows job statuses, printer health, and slot occupancy — metadata only.

---

## Pages

All routes under `/admin/*`. Protected by JWT with an `admin` role claim.

| Route | Content |
|---|---|
| `/admin` | Overview: mailbox status, queue depth |
| `/admin/jobs` | Job list with status and timestamps |
| `/admin/mailboxes` | Mailbox status + slot occupancy per unit |

---

## `/admin` — Overview

- Number of jobs in queue (pending/dispatching)
- Number of jobs completed today
- Mailbox status summary (online / printing / offline per unit)
- Last heartbeat timestamp per mailbox

Polling interval: 15 seconds (simple `setInterval` + fetch, no SSE needed here).

---

## `/admin/jobs` — Job List

Table view. Columns: `job_id` (truncated), `slot`, `status`, `page_count`, `submitted_at`, `delivered_at`.

No document content, no encrypted_key, no blob_ref shown in the UI.

Filter by status (all / pending / delivered / failed). Paginated — 50 rows per page.

---

## `/admin/mailboxes` — Mailbox Status

Per mailbox unit:
- Status badge: idle / printing / offline (offline = no heartbeat in last 2× interval)
- Last heartbeat timestamp
- Slot occupancy: text list — `Slot 1: 2/5`, `Slot 2: 0/5`, etc.
- Consumable alert stub: ink % and paper count fields (manually set for prototype, not wired to hardware)

---

## What This Is Not

- Not a fancy UI — plain table and text is fine
- No audit log viewer (the backend log is sufficient for the prototype demo)
- No consumable management workflow (alert stub only)
- No analytics or charts

The ops dashboard exists to prove the system is running end-to-end, not to be a polished product.
