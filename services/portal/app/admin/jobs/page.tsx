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
      <h1>Jobs</h1>

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
        <span className="muted">{total} total</span>
      </div>

      {error && <p className="error">{error}</p>}
      {loading && data === null && <p className="muted">Loading…</p>}
      {data && data.jobs.length === 0 && (
        <p className="muted">No jobs match this filter.</p>
      )}

      {data && data.jobs.length > 0 && (
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
                <td title={j.job_id}>{j.job_id.slice(0, 8)}…</td>
                <td>{j.slot_number}</td>
                <td>
                  <StatusBadge status={j.status} />
                </td>
                <td>{j.page_count}</td>
                <td>{new Date(j.created_at).toLocaleString()}</td>
                <td>
                  {j.delivered_at
                    ? new Date(j.delivered_at).toLocaleString()
                    : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
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
