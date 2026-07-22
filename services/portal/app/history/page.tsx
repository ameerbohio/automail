"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { StatusBadge } from "../admin/ui";
import { IconQueue, IconArrowRight } from "../icons";

interface HistoryJob {
  job_id: string;
  status: string;
  page_count: number;
  created_at: string;
  delivered_at?: string;
}

function shortDate(iso: string): string {
  return new Date(iso).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
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

  const delivered = jobs?.filter((j) => j.status === "delivered").length ?? 0;

  return (
    <main className="wrap-wide">
      <div className="page-head">
        <div>
          <p className="eyebrow">Account</p>
          <h1>Your mail</h1>
        </div>
        {jobs !== null && jobs.length > 0 && (
          <p className="muted">
            {jobs.length} sent &middot; {delivered} delivered
          </p>
        )}
      </div>

      {error && <p className="callout">{error}</p>}

      {jobs === null && !error && (
        <div className="skeleton-stack" aria-hidden="true">
          <div className="skeleton" style={{ width: "100%" }} />
          <div className="skeleton" style={{ width: "82%" }} />
          <div className="skeleton" style={{ width: "64%" }} />
        </div>
      )}

      {jobs !== null && jobs.length === 0 && (
        <div className="empty">
          <IconQueue size={40} />
          <p>Nothing posted yet.</p>
          <Link className="btn" href="/">
            Send a document
            <IconArrowRight size={16} />
          </Link>
        </div>
      )}

      {jobs !== null && jobs.length > 0 && (
        <div className="table-card">
          <table className="history">
            <thead>
              <tr>
                <th>Sent</th>
                <th>Pages</th>
                <th>Status</th>
                <th>Delivered</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((j) => (
                <tr key={j.job_id}>
                  {/* data-label feeds the stacked-card table layout below
                      640px (globals.css) -- same markup, no second render. */}
                  <td data-label="Sent">{shortDate(j.created_at)}</td>
                  <td data-label="Pages">{j.page_count}</td>
                  <td data-label="Status">
                    <StatusBadge status={j.status} />
                  </td>
                  <td data-label="Delivered">
                    {j.delivered_at ? shortDate(j.delivered_at) : "—"}
                  </td>
                  <td>
                    <Link href={`/jobs/${j.job_id}`}>track</Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}
