"use client";

import { useState, type FormEvent, type ChangeEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import {
  searchRecipients,
  getRecipientPublicKey,
  requestUploadURL,
  uploadBlob,
  createJob,
  type Recipient,
} from "@/lib/api";
import { encryptDocument, bufferToBase64 } from "@/lib/encrypt";
import { estimatePageCount } from "@/lib/pdf";
import { useAuth } from "@/lib/auth";
import {
  Postmark,
  IconSearch,
  IconUpload,
  IconLock,
  IconEyeOff,
  IconCheck,
  IconAlert,
  IconArrowRight,
  IconNode,
} from "./icons";

const MAX_SIZE_BYTES = 20 * 1024 * 1024; // 20 MB (server enforces this too)

interface Submitted {
  jobId: string;
  status: string;
  guestToken: string;
}

// Shows which of the cloud's N stateless nodes handled this submission
// (X-Automail-Node, see lib/proxy.ts). Two names here means the presign and
// the job-create landed on different nodes -- which is the point: the backend
// is a cluster, and no node holds session state that would stop it.
function ServedBy({ nodes }: { nodes: string[] }) {
  if (nodes.length === 0) return null;
  return (
    <p className="node-chip">
      <IconNode size={15} />
      <span>
        Routed via cloud node{nodes.length > 1 ? "s" : ""}{" "}
        {nodes.map((n, i) => (
          <span key={n}>
            {i > 0 && ", "}
            <code>{n}</code>
          </span>
        ))}
        {" — neither held your plaintext."}
      </span>
    </p>
  );
}

// Step chrome: a numbered stamp that turns into a tick once the step is
// satisfied, so the composer reads as a checklist without extra copy.
function StepHead({
  n,
  title,
  done,
}: {
  n: number;
  title: string;
  done: boolean;
}) {
  return (
    <div className="step-head">
      <span className="step-num">{done ? <IconCheck size={14} /> : n}</span>
      <h2>{title}</h2>
    </div>
  );
}

export default function Home() {
  const { accessToken, isAuthenticated } = useAuth();
  const router = useRouter();
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<Recipient[]>([]);
  const [selected, setSelected] = useState<Recipient | null>(null);
  const [file, setFile] = useState<File | null>(null);

  const [searching, setSearching] = useState(false);
  const [steps, setSteps] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState<Submitted | null>(null);
  const [nodes, setNodes] = useState<string[]>([]);

  function log(step: string) {
    setSteps((prev) => [...prev, step]);
  }

  // Record each distinct cloud node that answered during this submission.
  function noteNode(node?: string) {
    if (!node) return;
    setNodes((prev) => (prev.includes(node) ? prev : [...prev, node]));
  }

  async function onSearch(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (query.trim().length < 2) {
      setError("Enter at least 2 characters to search.");
      return;
    }
    setSearching(true);
    try {
      setResults(await searchRecipients(query.trim()));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Search failed.");
    } finally {
      setSearching(false);
    }
  }

  function onPickFile(e: ChangeEvent<HTMLInputElement>) {
    setError(null);
    const f = e.target.files?.[0] ?? null;
    if (f) {
      if (f.type !== "application/pdf") {
        setError("Only PDF files are accepted.");
        setFile(null);
        return;
      }
      if (f.size > MAX_SIZE_BYTES) {
        setError("File must be under 20 MB.");
        setFile(null);
        return;
      }
    }
    setFile(f);
  }

  async function onSubmit() {
    setError(null);
    if (!selected) {
      setError("Choose a recipient first.");
      return;
    }
    if (!file) {
      setError("Choose a PDF to send.");
      return;
    }

    setBusy(true);
    setSteps([]);
    setNodes([]);
    try {
      // 1. Fetch the recipient printer's public key.
      log("Fetching recipient encryption key…");
      const pubKeyPem = await getRecipientPublicKey(selected.recipient_id);

      // 2. Read + encrypt the PDF entirely in the browser.
      log("Encrypting document in your browser…");
      const pdfBuffer = await file.arrayBuffer();
      const pageCount = estimatePageCount(pdfBuffer);
      const { ciphertext, encryptedKey } = await encryptDocument(
        pdfBuffer,
        pubKeyPem,
      );

      // 3. Get a pre-signed upload URL and PUT the ciphertext straight to
      //    MinIO -- the cloud server never touches the blob.
      log("Requesting upload URL…");
      const { upload_url, blob_ref, served_by } = await requestUploadURL(
        selected.recipient_id,
        `${file.name}.enc`,
      );
      noteNode(served_by);
      log("Uploading encrypted blob…");
      await uploadBlob(upload_url, ciphertext);

      // 4. Submit the job. Logged in -> send the Bearer token (job stored with
      //    sender_id, no guest token). Guest -> no auth, server returns a
      //    one-time guest_token. Either way the AES key travels only as
      //    RSA-wrapped encrypted_key; the plaintext never left this tab.
      log("Submitting job…");
      const result = await createJob(
        {
          encrypted_key: bufferToBase64(encryptedKey),
          blob_ref,
          recipient_id: selected.recipient_id,
          page_count: pageCount,
        },
        accessToken,
      );
      noteNode(result.served_by);

      log("Done.");
      if (isAuthenticated) {
        // Authenticated senders track from their account -- no token to save.
        router.push(`/jobs/${result.job_id}`);
        return;
      }
      setSubmitted({
        jobId: result.job_id,
        status: result.status,
        guestToken: result.guest_token ?? "",
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Submission failed.");
    } finally {
      setBusy(false);
    }
  }

  /* ---------------------------------------------------- confirmation view */

  if (submitted) {
    return (
      <main className="wrap">
        <div className="postmark">
          <Postmark id="pm-accepted" center="ACCEPTED" size={124} />
        </div>

        <div style={{ textAlign: "center" }}>
          <h1>On its way</h1>
          <p className="lede" style={{ margin: "0.5rem auto 0" }}>
            Accepted as <strong className="ok">{submitted.status}</strong>. The
            document was sealed in this tab &mdash; nothing between here and the
            mailbox can open it.
          </p>
        </div>

        <div className="receipt">
          <p className="eyebrow">Retain for tracking</p>
          <p style={{ marginTop: "0.5rem" }}>
            <strong>Save this guest token.</strong> It is shown once and is the
            only way to track this job &mdash; there is no recovery if it is
            lost.
          </p>
          <code className="token">{submitted.guestToken}</code>
          <p className="muted">
            Job ID: <code>{submitted.jobId}</code>
          </p>
        </div>

        <ServedBy nodes={nodes} />

        <div className="action-row">
          <Link
            className="btn"
            href={`/track?job=${encodeURIComponent(
              submitted.jobId,
            )}&token=${encodeURIComponent(submitted.guestToken)}`}
          >
            Track this job
            <IconArrowRight size={16} />
          </Link>
          <button
            className="link"
            onClick={() => {
              setSubmitted(null);
              setFile(null);
              setSteps([]);
            }}
          >
            Send another
          </button>
        </div>
      </main>
    );
  }

  /* ---------------------------------------------------------- composer view */

  return (
    <main className="wrap">
      <section className="hero">
        <div className="hero-mark" aria-hidden="true">
          <Postmark id="pm-hero" size={260} center="SEALED" bottom="PAR AVION" />
        </div>

        <p className="eyebrow">Automail &middot; first class, end to end</p>
        <h1>Send a document straight to a mailbox.</h1>
        <p className="lede">
          Your PDF is encrypted in this browser before it is uploaded. The
          server routes ciphertext it has no key for; the mailbox unit decrypts
          in memory, prints, and wipes.
        </p>
        <ul className="assurances">
          <li className="assurance">
            <IconLock size={15} />
            Encrypted in this tab
          </li>
          <li className="assurance">
            <IconEyeOff size={15} />
            Server never sees plaintext
          </li>
          <li className="assurance">
            <IconCheck size={15} />
            Wiped after printing
          </li>
        </ul>
      </section>

      {/* Step 1: find a recipient */}
      <section
        className={`step${selected ? " is-done" : " is-ready"}`}
        style={{ "--i": 0 } as React.CSSProperties}
      >
        <StepHead n={1} title="Find a recipient" done={!!selected} />
        <form onSubmit={onSearch} className="row">
          <input
            type="text"
            placeholder="Name or building address"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Recipient search"
          />
          <button type="submit" disabled={searching}>
            {searching ? (
              "Searching…"
            ) : (
              <>
                <IconSearch size={16} />
                Search
              </>
            )}
          </button>
        </form>

        {results.length > 0 && (
          <ul className="results">
            {results.map((r, i) => (
              <li key={r.recipient_id} style={{ "--i": i } as React.CSSProperties}>
                <label>
                  <input
                    type="radio"
                    name="recipient"
                    checked={selected?.recipient_id === r.recipient_id}
                    onChange={() => setSelected(r)}
                  />
                  <span>
                    <span className="recipient-name">{r.display_name}</span>
                    <span className="recipient-addr">{r.building_address}</span>
                  </span>
                </label>
              </li>
            ))}
          </ul>
        )}
        {results.length === 0 && !searching && (
          <p className="muted" style={{ marginTop: "0.75rem" }}>
            Recipient names are masked until a job is accepted.
          </p>
        )}
      </section>

      {/* Step 2: choose a PDF */}
      <section
        className={`step${file ? " is-done" : selected ? " is-ready" : ""}`}
        style={{ "--i": 1 } as React.CSSProperties}
      >
        <StepHead n={2} title="Choose a PDF" done={!!file} />
        <label className={`dropzone${file ? " has-file" : ""}`}>
          <input type="file" accept="application/pdf" onChange={onPickFile} />
          <span className="dropzone-icon">
            {file ? <IconCheck size={19} /> : <IconUpload size={19} />}
          </span>
          <span>
            <span className="dropzone-title">
              {file ? file.name : "Drop a PDF here, or browse"}
            </span>
            <span className="muted" style={{ display: "block" }}>
              {file
                ? `${Math.ceil(file.size / 1024)} KB · ready to encrypt`
                : "PDF only, up to 20 MB"}
            </span>
          </span>
        </label>
      </section>

      {/* Step 3: encrypt + send */}
      <section
        className={`step${selected && file ? " is-ready" : ""}`}
        style={{ "--i": 2 } as React.CSSProperties}
      >
        <StepHead n={3} title="Encrypt & send" done={false} />
        <button
          className="btn btn-block"
          onClick={() => void onSubmit()}
          disabled={busy || !selected || !file}
        >
          {busy ? "Working…" : "Encrypt & send"}
        </button>

        {steps.length > 0 && (
          <ol className="tape">
            {steps.map((s, i) => (
              <li key={i}>{s}</li>
            ))}
          </ol>
        )}
        <ServedBy nodes={nodes} />
      </section>

      {error && (
        <p className="callout" role="alert">
          <IconAlert size={18} />
          <span>{error}</span>
        </p>
      )}

      <p className="muted" style={{ marginTop: "1.5rem" }}>
        Already sent something? <Link href="/track">Track a job &rarr;</Link>
      </p>
    </main>
  );
}
