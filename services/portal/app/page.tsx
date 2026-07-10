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

const MAX_SIZE_BYTES = 20 * 1024 * 1024; // 20 MB (server enforces this too)

interface Submitted {
  jobId: string;
  status: string;
  guestToken: string;
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

  function log(step: string) {
    setSteps((prev) => [...prev, step]);
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
      const { upload_url, blob_ref } = await requestUploadURL(
        selected.recipient_id,
        `${file.name}.enc`,
      );
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

  if (submitted) {
    return (
      <main className="wrap">
        <h1>Job submitted</h1>
        <p className="ok">
          Status: <strong>{submitted.status}</strong>
        </p>

        <div className="token-box">
          <p>
            <strong>Save this guest token.</strong> It is shown once and is the
            only way to track this job &mdash; there is no recovery if it is
            lost.
          </p>
          <code className="token">{submitted.guestToken}</code>
          <p className="muted">
            Job ID: <code>{submitted.jobId}</code>
          </p>
        </div>

        <p>
          <Link
            className="btn"
            href={`/track?job=${encodeURIComponent(
              submitted.jobId,
            )}&token=${encodeURIComponent(submitted.guestToken)}`}
          >
            Track this job &rarr;
          </Link>
        </p>
        <p>
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
        </p>
      </main>
    );
  }

  return (
    <main className="wrap">
      <h1>Send a document</h1>
      <p className="muted">
        Your PDF is encrypted in this browser before upload. The server only
        ever stores ciphertext.
      </p>

      {/* Step 1: find a recipient */}
      <section>
        <h2>1. Find a recipient</h2>
        <form onSubmit={onSearch} className="row">
          <input
            type="text"
            placeholder="Name or building address"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <button type="submit" disabled={searching}>
            {searching ? "Searching…" : "Search"}
          </button>
        </form>
        {results.length > 0 && (
          <ul className="results">
            {results.map((r) => (
              <li key={r.recipient_id}>
                <label>
                  <input
                    type="radio"
                    name="recipient"
                    checked={selected?.recipient_id === r.recipient_id}
                    onChange={() => setSelected(r)}
                  />
                  <span>
                    <strong>{r.display_name}</strong> &mdash;{" "}
                    {r.building_address}
                  </span>
                </label>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Step 2: choose a PDF */}
      <section>
        <h2>2. Choose a PDF</h2>
        <input type="file" accept="application/pdf" onChange={onPickFile} />
        {file && (
          <p className="muted">
            {file.name} ({Math.ceil(file.size / 1024)} KB)
          </p>
        )}
      </section>

      {/* Step 3: encrypt + send */}
      <section>
        <h2>3. Encrypt &amp; send</h2>
        <button
          className="btn"
          onClick={() => void onSubmit()}
          disabled={busy || !selected || !file}
        >
          {busy ? "Working…" : "Encrypt & send"}
        </button>
      </section>

      {steps.length > 0 && (
        <ol className="steps">
          {steps.map((s, i) => (
            <li key={i}>{s}</li>
          ))}
        </ol>
      )}
      {error && <p className="error">{error}</p>}

      <p className="muted">
        Already sent something? <Link href="/track">Track a job &rarr;</Link>
      </p>
    </main>
  );
}
