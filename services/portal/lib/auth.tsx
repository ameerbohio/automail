"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";

// Client-side auth state. Per plans/06-sender-portal.md the access token lives
// only in memory (never localStorage) to limit XSS blast radius; the refresh
// token is an HttpOnly cookie the browser manages. On load we bootstrap a new
// access token from that cookie (POST /api/auth/refresh), so a page reload
// keeps the session without persisting the JWT in JS-readable storage.

interface AuthState {
  accessToken: string | null;
  isAuthenticated: boolean;
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  register: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  // authFetch adds the Bearer header and, on a 401, transparently refreshes
  // the access token once and retries.
  authFetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
}

const AuthContext = createContext<AuthState | null>(null);

async function errorMessage(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body?.error) return body.error;
  } catch {
    // non-JSON body
  }
  return `request failed (${res.status})`;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [accessToken, setAccessToken] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let active = true;
    (async () => {
      try {
        const res = await fetch("/api/auth/refresh", { method: "POST" });
        if (active && res.ok) {
          const body = (await res.json()) as { access_token?: string };
          setAccessToken(body.access_token ?? null);
        }
      } catch {
        // no session / offline -- stay logged out
      } finally {
        if (active) setLoading(false);
      }
    })();
    return () => {
      active = false;
    };
  }, []);

  const login = useCallback(async (email: string, password: string) => {
    const res = await fetch("/api/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password }),
    });
    if (!res.ok) throw new Error(await errorMessage(res));
    const body = (await res.json()) as { access_token: string };
    setAccessToken(body.access_token);
  }, []);

  const register = useCallback(async (email: string, password: string) => {
    const res = await fetch("/api/auth/register", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password }),
    });
    if (!res.ok) throw new Error(await errorMessage(res));
    const body = (await res.json()) as { access_token: string };
    setAccessToken(body.access_token);
  }, []);

  const logout = useCallback(async () => {
    try {
      await fetch("/api/auth/logout", {
        method: "POST",
        headers: accessToken ? { Authorization: `Bearer ${accessToken}` } : {},
      });
    } finally {
      setAccessToken(null);
    }
  }, [accessToken]);

  const authFetch = useCallback(
    async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const withAuth = (token: string | null): RequestInit => ({
        ...init,
        headers: {
          ...((init.headers as Record<string, string>) ?? {}),
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
      });
      let res = await fetch(input, withAuth(accessToken));
      if (res.status === 401) {
        // Access token likely expired -- refresh once from the cookie, retry.
        const refreshed = await fetch("/api/auth/refresh", { method: "POST" });
        if (refreshed.ok) {
          const body = (await refreshed.json()) as { access_token?: string };
          const fresh = body.access_token ?? null;
          setAccessToken(fresh);
          res = await fetch(input, withAuth(fresh));
        }
      }
      return res;
    },
    [accessToken],
  );

  const value: AuthState = {
    accessToken,
    isAuthenticated: accessToken !== null,
    loading,
    login,
    register,
    logout,
    authFetch,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
