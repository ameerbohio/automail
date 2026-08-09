# 👋 Start here — project summaries

**You are in the right place.** This folder is the short version of the whole project, written for
someone who has a few minutes, not an afternoon.

Four pages. **About a minute each.** Every claim on them links to the code, the test, or the
recorded run behind it — nothing here asserts something the repository cannot show.

| Read this | If you care about |
|---|---|
| 🔐 **[Security & cryptography](security.md)** | The server routes mail it **cannot** decrypt — and the invariants guaranteeing it fail the build if violated |
| 🧪 **[Testing & quality](testing.md)** | 127 Go tests, fuzzing, a cross-language crypto contract, chaos scenarios, load gates, browser E2E |
| ☸️ **[Kubernetes & distributed systems](kubernetes.md)** | Rolling updates with zero dropped requests, autoscaling 2→7 pods, exactly-once job dispatch across nodes |
| 🖥️ **[Full-stack portal](portal.md)** | A Next.js/TypeScript app doing real cryptography in the browser, with live job status over SSE |

**In one sentence:** Automail is an end-to-end-encrypted physical-mail platform — a sender encrypts
a PDF in their browser, a zero-knowledge cloud server routes ciphertext it has no ability to read,
and a printer inside a mailbox unit decrypts in RAM, prints, and wipes it before reporting delivery.

New to the project? The [main README](../README.md) has the architecture diagram and how to run it.

---

### If you want to go deeper

| | |
|---|---|
| **The why behind each decision** | [`docs/study/`](../docs/study/) — 28 explainers, including the trade-offs that were rejected |
| **The specifications** | [`plans/`](../plans/) — 16 design docs, written *before* the code |
| **The receipts** | [`infra/k8s/RESULTS.md`](../infra/k8s/RESULTS.md) (measured cluster behaviour), [`scripts/load/baseline.json`](../scripts/load/baseline.json) (committed load baseline), [`docs/accepted-risks.md`](../docs/accepted-risks.md) (risks deliberately accepted, with re-review triggers) |
