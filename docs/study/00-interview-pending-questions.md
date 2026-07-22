# Interview Pending Questions

Side questions raised during interview prep that need to be answered before the interview. Do not answer in-session — research and fill in the answer here.

---

## Inaccuracies to correct (said something wrong during drill)

**RSA vs AES — "more secure" framing is wrong.**
Said: RSA is more secure than AES.
Correct: they solve different problems. RSA = asymmetric, solves key distribution. AES = symmetric, solves bulk encryption. Neither is more secure — they're used together because each covers what the other can't.

**"Document encrypted with RSA"**
Said: the printer decrypts the document with RSA.
Correct: RSA only wraps the AES key. The document is encrypted with AES. The printer uses RSA private key to unwrap the AES key, then uses AES to decrypt the document.

**SSE-S3 — "covers the case where attacker has the key"**
Said: SSE-S3 protects if an attacker has the key to decrypt the ciphertext.
Correct: the two layers use independent keys and defend independent threats. E2EE (browser AES key, never touches cloud) hides content from the operator. SSE-S3 (MinIO-managed key) protects against stolen physical media. They're not redundant because different adversary, different key.

**Redis Streams chosen for "scale"**
Said: Streams are needed due to scale of data.
Correct: the reason is at-least-once delivery with acknowledgement. Pub/Sub has no memory — if the subscriber is offline when the message fires, it's gone. Streams keep the entry in the PEL until XACK; it can be reclaimed if the consumer crashes.

---

## Hybrid Encryption

**Q: Why can't we just use AES directly to encrypt the PDF?**
*Context: asked during discussion of why hybrid RSA+AES was chosen over alternatives.*
Answer: _(to be filled in)_

---

## SELECT FOR UPDATE NOWAIT

**Q: What does `NOWAIT` do, and why use it over `SKIP LOCKED`?**
*Context: forgot during interview drill — study [15-select-for-update-nowait.md](15-select-for-update-nowait.md) before the interview.*
Answer: _(to be filled in after reading study doc)_

---

## Cross-node dispatch routing (fan-in)

**Q: The printer is connected via WebSocket to one cloud node. Why does Redis pub/sub still need to be involved when dispatching a job?**
*Context: forgot during interview drill — the pub/sub is between cloud nodes, not between cloud and printer. Study [11-dispatch-fan-in-printer-link.md](11-dispatch-fan-in-printer-link.md).*
Answer: _(to be filled in after reading study doc)_

---

## JWT RS256 vs HS256

**Q: What's the difference between RS256 and HS256, and why does RS256 matter for a multi-node server?**
*Context: forgot during interview drill. Study [03-jwt-rs256-vs-hs256.md](03-jwt-rs256-vs-hs256.md).*
Answer: _(to be filled in after reading study doc)_

---

## mTLS — why not a public CA?

**Q: Why can't you use Let's Encrypt instead of a self-signed internal CA for mTLS between cloud and printer?**
*Context: Let's Encrypt validates public domain ownership — printers have no domain. You control both ends so you be the CA. Study [05-mtls-internal-pki.md](05-mtls-internal-pki.md).*
Answer: _(to be filled in after reading study doc)_

---

## Guest token in the URL query string

**Q: The SSE stream authenticates guests with `?token=<guest_token>` — query strings end up in proxy/access logs and browser history. Why is that acceptable here, and what would the mitigation be at real scale?**
*Context: came up implementing Phase 5 auth. `EventSource` can't set request headers, which forces the token into the URL; the token is job-scoped, stored only as a SHA-256 hash, and job status is low-sensitivity metadata — but articulate the tradeoff and alternatives (short-lived signed URLs, a cookie set by a prior POST, fetch-based SSE polyfill that can send headers).*
Answer: _(to be filled in)_

---

## Constant-time comparison — when does it actually matter?

**Q: `authorizeStream` compares guest-token hashes with `subtle.ConstantTimeCompare`. When is a timing attack on a comparison real, and why is comparing *hashes* of an attacker-supplied value already mostly safe?**
*Context: came up implementing Phase 5. The attacker controls the preimage, not the hash, so leaking a byte-position of the hash mismatch doesn't let them converge on a valid token — but the constant-time habit is the defensible default. Be able to contrast with the case where it does matter (comparing raw secrets, e.g. HMAC signatures).*
Answer: _(to be filled in)_

---

## Padding oracle on the encrypted key file (CBC vs GCM)

**Q: The document is AES-256-**GCM** (authenticated), but the printer's private-key file is AES-256-**CBC** with PKCS#7 padding (that's what PBES2/`openssl genpkey` produces). CBC + PKCS#7 is the classic padding-oracle target. Why is it not a real threat for the key file, and when would decrypting CBC be dangerous?**
*Context: came up implementing Phase 6 key loading. Think about who supplies the ciphertext and whether an attacker gets a repeatable oracle (the key file is local, decrypted once at startup with a fixed passphrase — no attacker-chosen ciphertext, no oracle) vs. a network protocol that decrypts attacker-supplied CBC and reveals pad-valid/invalid.*
Answer: _(to be filled in)_

---

## OWNER DECISION — printer keepalive should refresh the liveness cache (Goal T7)

**Q: The dispatch-liveness cache `mailbox:<id>:state` has a 90s TTL, and
plans/05 frames "TTL expiry is the offline signal." But the printer's keepalive
sent only WebSocket *pings*, which never refresh that cache — so a
connected-but-idle printer dropped out of the dispatchable set after 90s and
jobs queued forever. T7's E2E surfaced this. I changed the keepalive to also
re-send a `state` frame each tick (30s < 90s), so a live socket keeps the cache
warm; plans/04 is titled "Keepalive *and State Reporting*," which supports this.
Confirm this is the intended model, and reconcile the wording in plans/05 (TTL =
offline) with plans/04 (keepalive reports state). Alternative designs: refresh
the TTL on the cloud side when a pong arrives (keeps the printer dumber), or
shorten/lengthen the TTL relative to the heartbeat interval.**
*Context: this is a printer+cloud change made inside the portal-E2E goal because
it blocked the acceptance criteria — a clear-cut dispatch-liveness regression,
not a portal concern. Flagged here for owner sign-off since it touches the
distributed-systems design, not just tests. See docs/study/17 "Browser E2E".*
Answer: _(to be filled in)_

---

## Why keep the RSA key resident but zero the passphrase?

**Q: Phase 6 zeroes the key passphrase seconds after startup but leaves the RSA private key sitting in RAM for the whole process lifetime. Isn't the private key the more sensitive secret? Why the asymmetry?**
*Context: came up implementing Phase 6 memory hygiene. The RSA key is needed to unwrap the AES key on every job, so it must stay resident; the passphrase's only job is the one-time key load, so it can go immediately. Be able to discuss what production would do differently (HSM / locked memory pages / `memguard`) and why zeroing a Go `string` from `os.Getenv` is impossible anyway.*
Answer: _(to be filled in)_

---

---

## OWNER DECISION — SSE fan-out opens one Redis subscription per subscriber (Goal T10)

**Q: `StreamJob` (handlers/jobs.go) calls `s.Redis.Subscribe("job:<id>:status")`
once per client connection. So N browser tabs watching the *same* job open N
Redis subscriptions and N server goroutines — fan-out is O(N), not O(1). Goal
T10's load test confirms this is *bounded* (goroutines return to baseline when
clients disconnect — no leak), which is the correctness bar. But the plan's
interview framing says "SSE fan-out is bounded because subscribers share one
Redis subscription per job," which the code does NOT currently do. Should we
(a) leave it as-is (simple, and N-per-job is fine at this scale), or (b)
implement a per-node, per-job fan-out hub: one Redis subscription per job on
each node, multiplexing to all local subscribers of that job? Option (b) is the
textbook answer and cuts Redis connections from O(subscribers) to O(jobs·nodes),
but adds a concurrency-managed registry (subscribe on first local watcher,
unsubscribe on last) that must not leak or race.**
*Context: found while implementing the Part 8 load suite. Not a bug — the design
is leak-free and correct — but a real scaling design decision, and the plan's
own words describe option (b). Flagged for the owner rather than silently
refactored (touches the SSE hot path). See docs/study/17 "Performance & load".*
Answer: _(to be filled in)_
