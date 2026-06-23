# Asset Management & Field Maintenance

**Language**: Go (telemetry ingest) + TypeScript / Next.js (fleet console)
**Tag**: [STRETCH] (telemetry stub + alerting demoable; full fleet console is production scope)
**Role**: Keep 12M physical units healthy. Ingest device diagnostics, detect faults, and dispatch maintenance to field technicians before a unit fails to deliver mail.

This is **not** the ops dashboard ([07-ops-dashboard.md](07-ops-dashboard.md)). That is property-manager-facing and shows job/printer *metadata* for one building. This is **Automail-team-facing** and shows *physical health* across the entire fleet, plus the maintenance workflow. Neither ever sees document content.

---

## Why It Exists (the practical need)

At 12M units serving ~30M people, mail delivery depends on hardware that jams, runs out of consumables, overheats, loses connectivity, or gets tampered with. A failed unit silently stops delivering an entire building's mail. The system must **detect a fault and get a technician on site fast** — diagnostics-driven maintenance is the difference between a 30-minute fix and a building going days without mail. This also bounds operating cost: targeted dispatch (vs scheduled truck-rolls) is the cheaper way to maintain a fleet this size, and ties into the Canada Post carrier role (carriers deliver consumables/spares; Automail field techs handle complex faults — see [00-overview.md](00-overview.md)).

---

## Device Diagnostics (telemetry)

Each unit reports health on the **same transport as job state** — no new connection. In demo `push` mode the `state` frame carries an optional `diagnostics` block; in production `poll` mode telemetry piggybacks on the poll (see [01-architecture.md](01-architecture.md) "Dispatch Transport at Scale"). Reusing the channel keeps the device's network/power profile unchanged.

Reported fields (best-effort, sampled):

```json
{ "type": "state", "status": "idle",
  "slot_occupancy": { "...": { "current": 2, "max": 5 } },
  "diagnostics": {
    "firmware": "1.4.2",
    "uptime_s": days,
    "printer": { "state": "ok|jam|error", "code": "..." },
    "consumables": { "paper_pct": 38, "toner_pct": 12, "envelopes": 140 },
    "mechanism": { "fold_seal_faults": 0, "slot_actuator": "ok|stuck" },
    "environment": { "temp_c": 41, "door": "closed|open", "tamper": false },
    "connectivity": { "rssi_dbm": -67, "last_sync_s": 30 }
  } }
```

Diagnostics are **operational metadata only** — no PII, no document bytes. They follow the same zero-knowledge boundary as everything else on the link.

---

## Ingest & Fault Detection

```
device telemetry → telemetry ingest (Go) → time-series store (per-unit, downsampled)
                                          → rule engine → open/upgrade maintenance ticket
```

- **Storage at scale:** raw per-unit telemetry for 12M units is too much to keep hot. Store latest snapshot + downsampled history; retain full detail only around fault events.
- **Rule engine (examples):** `toner_pct < 15` → consumable ticket (low severity); `printer.state = jam` for > N polls → on-site ticket (medium); `tamper = true` or `temp_c` over threshold → urgent ticket; `last_sync` exceeds 2× expected interval → **offline** ticket (the missed-poll / dropped-socket signal already exists in both transport modes).
- **Dedup:** one open ticket per (unit, fault class); telemetry updates the ticket rather than spawning duplicates.

---

## Maintenance Tickets & Dispatch

Ticket lifecycle: `open → assigned → en_route → on_site → resolved` (or `auto_resolved` if telemetry recovers, e.g. consumable refilled).

- **Severity → SLA:** urgent (tamper/thermal/offline-building) vs routine (consumables) drive response targets and routing.
- **Assignment:** ticket routed to the nearest available field technician (or to Canada Post carrier for consumable-only restocks). Carries unit location, fault class, and last diagnostics snapshot so the tech arrives prepared.
- **Notification:** push to a technician app / on-call channel; escalate if unacknowledged.
- **Audit:** ticket state changes are written to the immutable audit log alongside job events.

---

## Fleet Console (Automail team)

Separate app/route group from the ops dashboard, gated to Automail staff roles.

| View | Content |
|---|---|
| Fleet overview | counts by health state (healthy / degraded / offline), open tickets by severity, map or region rollup |
| Unit detail | live diagnostics, telemetry history, ticket history, firmware version |
| Ticket queue | open/assigned tickets, severity filter, assignment + status |
| Alerts | active faults breaching SLA |

At fleet scale the overview is **aggregate-first** (region/health rollups), drilling into a unit only on demand — rendering 12M rows directly is a non-goal.

---

## Data Model (additions)

Extends [08-data-models.md](08-data-models.md):

```sql
-- latest snapshot per unit (hot path; one row per unit)
device_health   (mailbox_id PK→mailboxes, diagnostics JSONB, health TEXT, reported_at)
-- downsampled history + event-window detail
device_telemetry(id, mailbox_id, sampled_at, metrics JSONB)
-- maintenance workflow
maintenance_tickets(id, mailbox_id, fault_class, severity, status,
                    assigned_to, opened_at, resolved_at, last_snapshot JSONB)
```

---

## Demo vs Production

- **Demo:** the printer emits a `diagnostics` block (values stubbed / hand-set like the consumable stub in [07-ops-dashboard.md](07-ops-dashboard.md)); ingest stores it; one or two rules fire and open a ticket visible in a minimal fleet view. Proves the loop end-to-end.
- **Production:** time-series storage, full rule engine, technician routing/SLAs, firmware/OTA correlation ([01-architecture.md](01-architecture.md) cert rotation / enrollment), and region-scale aggregation.
