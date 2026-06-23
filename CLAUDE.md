# Automail — Agent Instructions

Automail is an end-to-end-encrypted automated mail system: senders upload encrypted PDFs, a zero-knowledge cloud server routes ciphertext, and a printer microservice inside a **mailbox unit** decrypts in RAM, prints, and wipes. Resume/portfolio project — the security and distributed-systems depth is the point.

## Source of truth
The `plans/` directory is the specification. Read the relevant plan before implementing. Build order and per-phase acceptance criteria live in [plans/10-implementation-roadmap.md](plans/10-implementation-roadmap.md) — each phase has a **"Verify:"** line that defines done.

## Working relationship
The author implements and owns the design; the agent teaches and drafts. Explain the *why* (interview prep — the author studies this code). Do not silently expand scope. See [plans/11-ai-collaboration.md](plans/11-ai-collaboration.md).

## Non-negotiable security invariants
- **Zero-knowledge cloud server.** It only ever stores/forwards ciphertext + metadata. Never call any decryption on `encrypted_key`; never log it; never pass it anywhere but the printer link.
- **Plaintext lives only in printer RAM + tmpfs** (`/dev/shm`), unlinked before the status callback, then zeroed. No plaintext to disk, logs, or network.
- **mTLS on every internal hop**, including the dial-out printer WebSocket link.
- The physical unit's identity is **`mailbox_id`** (table `mailboxes`, Redis `mailbox:<id>:*`). "Printer microservice" is the software that prints inside a mailbox unit.

## Workflow per phase
1. Implement exactly one roadmap phase (use the `phase-implementer` agent).
2. Run the `plan-checker` agent — it verifies the working tree against the plans + that phase's "Verify:" criteria.
3. Fix any drift, then commit (one phase per commit).

## Study docs
For each non-trivial concept implemented, add/update a short interview-oriented explainer under `docs/study/` (see `docs/study/README.md`). This is a deliverable, not optional.

## Conventions
- **Commits:** clean messages, subject + body. **No AI co-author trailer.**
- **Environment:** developed in WSL2, deployed to Proxmox+Docker. Runs Linux/Docker/CUPS. On Windows, stay in `DEV_MODE=true`.
- **Secrets** (`.env`, `*.pem`, `certs/`) are gitignored — never commit them.
