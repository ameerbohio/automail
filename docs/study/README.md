# Study Notes (interview prep)

One file per concept, written to be re-read before an interview. As each phase is implemented, the implementer adds/updates the relevant note here. These explain the *why*, not just the *what* — they are the narrated version of the code.

## Convention
- File name: `NN-concept.md` (kebab-case), grouped loosely by area.
- Structure each note as: **What it is → Why we chose it (the tradeoff/alternative) → The honest caveat (the follow-up an interviewer asks).**
- Link back to the plan and the code: `plans/05-cloud-server.md`, `services/cloud/...`.
- Keep the symmetric/contrast framings explicit (e.g. AES symmetric vs RSA asymmetric; SSE fan-out vs printer-link fan-in) — they are the high-value recall hooks.

## Seed topics (from plans/11-ai-collaboration.md flashcards)
- Hybrid encryption: RSA-OAEP wraps the AES-256-GCM key (asymmetric ships the key, symmetric ships the document).
- JWT RS256 vs HS256 — asymmetric signing for an N-node stateless cluster.
- Redis Streams + consumer groups vs Pub/Sub (at-least-once + idempotency).
- SELECT FOR UPDATE NOWAIT — authoritative double-dispatch guard.
- MinIO SSE-S3 as a second, independent-key encryption layer.
- Audit-log immutability via Postgres trigger (+ privilege separation, hash-chaining).
- Dispatch fan-in: printer dials out, cross-node routing via Redis (mirror of SSE fan-out).
- Push vs poll transport — latency vs power at 12M-mailbox scale.

Write the note when you implement the concept, not before.
