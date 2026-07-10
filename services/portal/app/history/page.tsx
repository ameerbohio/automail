"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";

interface HistoryJob {
  job_id: string;
  status: string;
  page_count: number;
  created_at: string;
  delivered_at?: string;
}

export default function HistoryPage() {
  const { authFetch, loading, isAuthenticated } = useAuth();
  const router = useRouter();
  const [jobs, setJobs] = useState<HistoryJob[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (loading) return; // wait for the session bootstrap
    if (!isAuthenticated) {
      router.replace("/login?next=/history");
      return;
    }
    let active = true;
    (async () => {
      try {
        const res = await authFetch("/api/jobs");
        if (res.status === 401) {
          router.replace("/login?next=/history");
          return;
        }
        if (!res.ok) throw new Error(`could not load history (${res.status})`);
        const body = (await res.json()) as { jobs: HistoryJob[] };
        if (active) setJobs(body.jobs);
      } catch (err) {
        if (active) {
          setError(err instanceof Error ? err.message : "Failed to load history.");
        }
      }
    })();
    return () => {
      active = false;
    };
  }, [loading, isAuthenticated, authFetch, router]);

  return (
    <main className="wrap">
      <h1>Your mail</h1>
      {error && <p className="error">{error}</p>}
      {jobs === null && !error && <p className="muted">Loading…</p>}
      {jobs !== null && jobs.length === 0 && (
        <p className="muted">
          You haven&rsquo;t sent anything yet.{" "}
          <Link href="/">Send a document &rarr;</Link>
        </p>
      )}
      {jobs !== null && jobs.length > 0 && (
        <table className="history">
          <thead>
            <tr>
              <th>Sent</th>
              <th>Pages</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr key={j.job_id}>
                <td>{new Date(j.created_at).toLocaleString()}</td>
                <td>{j.page_count}</td>
                <td>{j.status}</td>
                <td>
                  <Link href={`/jobs/${j.job_id}`}>track</Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
