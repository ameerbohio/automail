# Automail — Overview

## Problem

Physical mail delivery is getting expensive as time goes with falling demand in physical mail and increasing addresses to service at full capacity. Canada Post's delivery cost is dominated by per-address stops, a carrier visits hundreds of individual units per route, delivering one or two pieces of mail each. The unit economics do not improve at scale. There are issues of the governments demands on Canada Post still remaining intense (and outdated) as mentioned in their 2024 Annual Report. A solution needs to be found that can satisfy the business and the union. No risk can be taken for replacing workers with automated solutions.

## Solution

Automail is an automated mail processing unit installed into the existing shared mailbox area of a residential or condo building or community mailboxes. Senders (individuals, businesses, government agencies) upload documents digitally trhough a secure portal to the cloud (encrypted). The cloud determines which mailbox to send the request to, then the unit prints, folds, seals, and deposits each piece of mail into the correct mailbox slot without a carrier ever touching the mail.

Canada Post's role shifts: instead of delivering paper mail per-address required by the Canadian Postal Charter, carriers deliver bulk consumable restocks (paper, ink, envelopes) to each building unit on a less frequent schedule based on metrics reported by each mailbox and would take on more parcel deliveries, maintaining a carrier's route ownership (a demand of the union). Mail of flyers and advertising would still remain under carrier deliveries since companies would like prefer to keep their style of advertising mail based on their marketting team, not limiting to simple printed mail, although this would open up as a cheaper alternative to advertise by simply uploading a well designed letter.

While there are practical limitations to this solution like whether or not this can be agreed upon in the face of demands like "owning routes" for postal service workers where changing routes to begin with have heavy process and approvals behind it, or another issue where workers end up not having work since its taken care of by automail and parcel business does not ramp up fast enough to compensate, the technical solution is available under the other restrictions mentioned above. The maintenance for these machines can be partially handled by Canada Post where carriers can also "deliver" spare parts as part of their role and fix minor issues while the field engineers of Automail can also debug systems on site if an issue is complex, everyone's happy (optimistically).

## Prototype Scope

This prototype demonstrates the full software stack, with the home printer simulating the on-site unit. Physical robotics and hardware integration are out of scope.

**In scope:**
- End-to-end encrypted document upload (zero-knowledge cloud server because sensitive mail)
- Job dispatch to a printer microservice (simulates the on-site unit, would be a microcontroller connected to the internet instead)
- Real-time job status via SSE
- Stateless cloud server horizontally scalable via Redis Streams
- Sender portal (TypeScript / Next.js)
- Ops dashboard (minimal, the interface for Canada Post and Automail)
- Billing system with Stripe test mode (per-page pricing, append-only ledger, cost estimate before submit, payment intent lifecycle tied to job delivery — no real charges)

- Asset-management telemetry stub: units report hardware diagnostics; a rule opens a maintenance ticket for field technicians (full fleet console is production scope — see [12-asset-management.md](12-asset-management.md))

**Out of scope for prototype:**
- Physical printer robotics, mailbox actuation
- Canada Post API integration
- Billing in live/production mode (Stripe live keys)
- Multi-building routing at scale
- Mobile app

## Target Audience

Canada Post, other postal service companies that struggle having to deal with transactional mail in the world.
