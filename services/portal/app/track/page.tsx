"use client";

import { useEffect, useRef, useState, type FormEvent } from "react";
import Link from "next/link";

// The status ladder a job climbs (plans/09-api-contracts.md). Used only to
// render progress; the server is the source of truth for the actual value.
const LADDER = ["submitted", "queued", "dispatching", "printing", "delivered"];

function isTerminal(status: string): boolean {
  return status === "delivered" || status === "failed";
}

export default function TrackPage() {
  const [jobId, setJobId] = useState("");
  const [token, setToken] = useState("");
  const [statuses, setStatuses] = useState<string[]>([]);
  const [current, setCurrent] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  // Pre-fill from the query string (?job=…&token=…) that the submit page
  // links to. Read from window in an effect rather than useSearchParams to
  // keep this a plain client page with no Suspense/prerender constraints.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const j = params.get("job");
    const t = params.get("token");
    if (j) setJobId(j);
    if (t) setToken(t);
  }, []);

  // Always close any open stream when the component unmounts.
  useEffect(() => {
    return () => esRef.current?.close();
  }, []);

  function stop() {
    esRef.current?.close();
    esRef.current = null;
    setConnected(false);
  }

  function track(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!jobId.trim() || !token.trim()) {
      setError("Enter both the job ID and the guest token.");
      return;
    }

    stop();
    setStatuses([]);
    setCurrent(null);

    // Guest auth: the token rides in the query string because EventSource
    // cannot set an Authorization header (plans/09-api-contracts.md).
    const es = new EventSource(
      `/api/jobs/${encodeURIComponent(jobId.trim())}/stream?token=${encodeURIComponent(
        token.trim(),
      )}`,
    );
    esRef.current = es;
    setConnected(true);

    es.onmessage = (ev) => {
      try {
        const data = JSON.parse(ev.data) as { status: string; error?: string };
        setCurrent(data.status);
        setStatuses((prev) => [...prev, data.status]);
        if (data.status === "failed" && data.error) {
          setError(data.error);
        }
        if (isTerminal(data.status)) {
          stop();
        }
      } catch {
        // ignore malformed frames
      }
    };

    es.onerror = () => {
      // EventSource fires onerror on network failure or when the server closes
      // the stream. If we already reached a terminal status this is the normal
      // close; otherwise surface a connection problem.
      if (!esRef.current) return; // already stopped intentionally
      setError((prev) => prev ?? "Connection lost. Check the job ID and token.");
      stop();
    };
  }

  const currentIndex = current ? LADDER.indexOf(current) : -1;

  return (
    <main className="wrap">
      <h1>Track a job</h1>
      <p className="muted">
        Enter the job ID and guest token you saved when you submitted.
      </p>

      <form onSubmit={track}>
        <label className="field">
          Job ID
          <input
            type="text"
            value={jobId}
            onChange={(e) => setJobId(e.target.value)}
            placeholder="uuid"
          />
        </label>
        <label className="field">
          Guest token
          <input
            type="text"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="one-time token"
          />
        </label>
        <button className="btn" type="submit">
          {connected ? "Reconnect" : "Track"}
        </button>
      </form>

      {current && (
        <section className="status">
          <p>
            Current status: <strong>{current}</strong>
            {current === "failed" && " ✗"}
            {current === "delivered" && " ✓"}
          </p>
          <ol className="ladder">
            {LADDER.map((s, i) => (
              <li
                key={s}
                className={
                  current === "failed"
                    ? "pending"
                    : i <= currentIndex
                      ? "reached"
                      : "pending"
                }
              >
                {s}
              </li>
            ))}
          </ol>
          <details>
            <summary>Event log ({statuses.length})</summary>
            <ol className="steps">
              {statuses.map((s, i) => (
                <li key={i}>{s}</li>
              ))}
            </ol>
          </details>
        </section>
      )}

      {error && <p className="error">{error}</p>}

      <p className="muted">
        <Link href="/">&larr; Send another document</Link>
      </p>
    </main>
  );
}
