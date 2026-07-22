"use client";

import { useEffect, useRef, useState, type FormEvent } from "react";
import Link from "next/link";
import { JobProgress, DeliveredStamp, isTerminal } from "../journey";
import { IconAlert, IconArrowRight } from "../icons";

function stamp(): string {
  return new Date().toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export default function TrackPage() {
  const [jobId, setJobId] = useState("");
  const [token, setToken] = useState("");
  const [statuses, setStatuses] = useState<string[]>([]);
  const [times, setTimes] = useState<Record<string, string>>({});
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
    setTimes({});
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
        // First sighting of a status wins, so the tracker shows when the job
        // *entered* each stage rather than when it last repeated it.
        setTimes((prev) =>
          prev[data.status] ? prev : { ...prev, [data.status]: stamp() },
        );
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

  const delivered = current === "delivered";
  const failed = current === "failed";

  return (
    <main className="wrap">
      <p className="eyebrow">Guest tracking</p>
      <h1>Track a job</h1>
      <p className="lede" style={{ margin: "0.5rem 0 1.5rem" }}>
        Enter the job ID and the one-time guest token you saved when you
        submitted. Status is streamed live as the document moves.
      </p>

      <form onSubmit={track} className="card">
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
          <p className="status-line">
            <span
              className={`live-dot${delivered ? " is-done" : ""}${failed ? " is-failed" : ""}`}
              aria-hidden="true"
            />
            Current status: <strong>{current}</strong>
          </p>

          <JobProgress current={current} times={times} />

          {delivered && <DeliveredStamp at={times["delivered"]} />}

          <details className="log-details">
            <summary>Event log ({statuses.length})</summary>
            <ol className="tape">
              {statuses.map((s, i) => (
                <li key={i}>{s}</li>
              ))}
            </ol>
          </details>
        </section>
      )}

      {error && (
        <p className="callout" role="alert">
          <IconAlert size={18} />
          <span>{error}</span>
        </p>
      )}

      <p className="muted" style={{ marginTop: "2rem" }}>
        <Link href="/">Send another document</Link>{" "}
        <IconArrowRight size={14} style={{ display: "inline", verticalAlign: "-2px" }} />
      </p>
    </main>
  );
}
