"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { JobProgress, DeliveredStamp, isTerminal } from "../../journey";
import { IconAlert } from "../../icons";

function stamp(): string {
  return new Date().toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export default function JobStatusPage() {
  const params = useParams<{ id: string }>();
  const jobId = params.id;
  const { accessToken, loading, isAuthenticated } = useAuth();
  const router = useRouter();
  const [current, setCurrent] = useState<string | null>(null);
  const [times, setTimes] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    if (loading) return;
    if (!isAuthenticated || !accessToken) {
      router.replace(`/login?next=/jobs/${jobId}`);
      return;
    }

    // Authenticated SSE: EventSource can't set an Authorization header, so the
    // short-lived access token rides in the query string; the proxy turns it
    // into a Bearer and the cloud does the JWT ownership check.
    const es = new EventSource(
      `/api/jobs/${encodeURIComponent(jobId)}/stream?access=${encodeURIComponent(
        accessToken,
      )}`,
    );
    esRef.current = es;

    es.onmessage = (ev) => {
      try {
        const data = JSON.parse(ev.data) as { status: string; error?: string };
        setCurrent(data.status);
        // Keep the first time we saw each status -- that is the arrival time.
        setTimes((prev) =>
          prev[data.status] ? prev : { ...prev, [data.status]: stamp() },
        );
        if (data.status === "failed" && data.error) setError(data.error);
        if (isTerminal(data.status)) es.close();
      } catch {
        // ignore malformed frames
      }
    };
    es.onerror = () => {
      if (esRef.current) {
        setError((prev) => prev ?? "Connection lost.");
        es.close();
      }
    };

    return () => {
      esRef.current = null;
      es.close();
    };
  }, [loading, isAuthenticated, accessToken, jobId, router]);

  const delivered = current === "delivered";
  const failed = current === "failed";

  return (
    <main className="wrap">
      <p className="eyebrow">Job</p>
      <h1>In transit</h1>
      <p className="muted" style={{ marginTop: "0.375rem" }}>
        <code>{jobId}</code>
      </p>

      {current ? (
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
        </section>
      ) : (
        // Before the first SSE frame lands, show the rail with nothing on it
        // rather than a bare "Connecting…" -- the shape of the page is stable.
        !error && (
          <section className="status">
            <p className="status-line">
              <span className="live-dot" aria-hidden="true" />
              Opening live stream…
            </p>
            <JobProgress current={null} />
          </section>
        )
      )}

      {error && (
        <p className="callout" role="alert">
          <IconAlert size={18} />
          <span>{error}</span>
        </p>
      )}

      <p className="muted" style={{ marginTop: "2rem" }}>
        <Link href="/history">&larr; Back to your mail</Link>
      </p>
    </main>
  );
}
