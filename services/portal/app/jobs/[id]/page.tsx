"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";

const LADDER = ["submitted", "queued", "dispatching", "printing", "delivered"];

function isTerminal(status: string): boolean {
  return status === "delivered" || status === "failed";
}

export default function JobStatusPage() {
  const params = useParams<{ id: string }>();
  const jobId = params.id;
  const { accessToken, loading, isAuthenticated } = useAuth();
  const router = useRouter();
  const [current, setCurrent] = useState<string | null>(null);
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

  const currentIndex = current ? LADDER.indexOf(current) : -1;

  return (
    <main className="wrap">
      <h1>Job status</h1>
      <p className="muted">
        Job ID: <code>{jobId}</code>
      </p>

      {current ? (
        <section className="status">
          <p>
            Current status: <strong>{current}</strong>
            {current === "delivered" && " ✓"}
            {current === "failed" && " ✗"}
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
        </section>
      ) : (
        !error && <p className="muted">Connecting…</p>
      )}

      {error && <p className="error">{error}</p>}

      <p className="muted">
        <Link href="/history">&larr; Back to your mail</Link>
      </p>
    </main>
  );
}
