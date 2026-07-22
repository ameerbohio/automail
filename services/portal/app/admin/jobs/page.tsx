"use client";

import { useState } from "react";
import { useAdminData } from "@/lib/admin";
import { NotAuthorized, StatusBadge } from "../ui";

// /admin/jobs (plans/07): operator job table with a status filter and 50-per-
// page pagination. Metadata only -- the cloud endpoint returns no ciphertext,
// and there is nothing here that could show document content.
const PER_PAGE = 50;

// The real job statuses in lifecycle order (plans/08). "All" = no filter.
// plans/07 lists an illustrative set; the exact-match filter uses the actual
// status values, including the schema-default 'submitted' so freshly-queued
// jobs are filterable and not only visible under "All".
const STATUSES = [
  "submitted",
  "queued",
  "dispatching",
  "printing",
  "delivered",
  "failed",
] as const;

interface AdminJob {
  job_id: string;
  slot_number: number;
  status: string;
  page_count: number;
  created_at: string;
  delivered_at?: string;
}

interface JobsResponse {
  jobs: AdminJob[];
  total: number;
  page: number;
}

export default function AdminJobs() {
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(1);

  const query = new URLSearchParams({
    page: String(page),
    per_page: String(PER_PAGE),
  });
  if (status) query.set("status", status);
  const { data, error, forbidden, loading } = useAdminData<JobsResponse>(
    `/api/admin/jobs?${query.toString()}`,
  );

  if (forbidden) return <NotAuthorized />;

  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PER_PAGE));

  function onFilter(next: string) {
    setStatus(next);
    setPage(1); // a new filter resets to the first page
  }

  return (
    <main>
      <div className="page-head">
        <h1>Jobs</h1>
        <p className="muted">{total} total</p>
      </div>

      <div className="filters">
        <label className="field" style={{ margin: 0 }}>
          Status
          <select
            value={status}
            onChange={(e) => onFilter(e.target.value)}
            style={{ marginLeft: "0.5rem" }}
          >
            <option value="">All</option>
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="callout">{error}</p>}
      {loading && data === null && (
        <div className="skeleton-stack" aria-hidden="true">
          <div className="skeleton" />
          <div className="skeleton" style={{ width: "88%" }} />
          <div className="skeleton" style={{ width: "72%" }} />
        </div>
      )}
      {data && data.jobs.length === 0 && (
        <div className="empty">
          <p>No jobs match this filter.</p>
        </div>
      )}

      {data && data.jobs.length > 0 && (
        <div className="table-card">
          <table className="history">
            <thead>
              <tr>
                <th>Job</th>
                <th>Slot</th>
                <th>Status</th>
                <th>Pages</th>
                <th>Submitted</th>
                <th>Delivered</th>
              </tr>
            </thead>
            <tbody>
              {data.jobs.map((j) => (
                <tr key={j.job_id}>
                  {/* Truncated id with the full uuid on the title attribute --
                      operators copy it out of the tooltip. */}
                  <td className="cell-mono" data-label="Job" title={j.job_id}>
                    {j.job_id.slice(0, 8)}…
                  </td>
                  <td data-label="Slot">{j.slot_number}</td>
                  <td data-label="Status">
                    <StatusBadge status={j.status} />
                  </td>
                  <td data-label="Pages">{j.page_count}</td>
                  <td data-label="Submitted">
                    {new Date(j.created_at).toLocaleString()}
                  </td>
                  <td data-label="Delivered">
                    {j.delivered_at
                      ? new Date(j.delivered_at).toLocaleString()
                      : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="pager">
        <button disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
          ‹ Prev
        </button>
        <span className="muted">
          Page {page} of {totalPages}
        </span>
        <button
          disabled={page >= totalPages}
          onClick={() => setPage((p) => p + 1)}
        >
          Next ›
        </button>
      </div>
    </main>
  );
}
