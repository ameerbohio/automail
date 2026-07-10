"use client";

import { useCallback, useEffect, useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";

// Shared client for the admin (ops-dashboard) pages. Every admin view is the
// same shape: an authenticated GET whose response is JSON, guarded upstream by
// requireAdmin. This hook centralizes the three outcomes those pages must
// distinguish:
//   - 401 (no / expired session)   -> bounce to login, preserving where we were
//   - 403 (logged in, not an admin) -> render a friendly "not authorized" note
//   - 200                           -> data
// authFetch (lib/auth) already adds the Bearer header and transparently
// refreshes once on a 401 before we ever see it here.

export interface AdminData<T> {
  data: T | null;
  error: string | null;
  forbidden: boolean;
  loading: boolean;
  reload: () => void;
}

export function useAdminData<T>(path: string, pollMs?: number): AdminData<T> {
  const { authFetch, loading: authLoading, isAuthenticated } = useAuth();
  const router = useRouter();
  const pathname = usePathname();
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [loading, setLoading] = useState(true);

  const toLogin = useCallback(() => {
    router.replace(`/login?next=${encodeURIComponent(pathname)}`);
  }, [router, pathname]);

  const load = useCallback(async () => {
    try {
      const res = await authFetch(path);
      if (res.status === 401) {
        toLogin();
        return;
      }
      if (res.status === 403) {
        setForbidden(true);
        setError(null);
        setLoading(false);
        return;
      }
      if (!res.ok) throw new Error(`request failed (${res.status})`);
      const body = (await res.json()) as T;
      setData(body);
      setForbidden(false);
      setError(null);
      setLoading(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load.");
      setLoading(false);
    }
  }, [authFetch, path, toLogin]);

  useEffect(() => {
    if (authLoading) return; // wait for the session bootstrap
    if (!isAuthenticated) {
      toLogin();
      return;
    }
    let active = true;
    void load();
    if (!pollMs) return;
    // plans/07: the dashboard refreshes on a simple interval, no SSE.
    const id = setInterval(() => {
      if (active) void load();
    }, pollMs);
    return () => {
      active = false;
      clearInterval(id);
    };
  }, [authLoading, isAuthenticated, load, pollMs, toLogin]);

  return { data, error, forbidden, loading, reload: load };
}
