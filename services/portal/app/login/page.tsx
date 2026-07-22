"use client";

import { useState, type FormEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { Logo, IconAlert } from "../icons";

export default function LoginPage() {
  const { login } = useAuth();
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await login(email, password);
      // Read ?next= from the URL (set by middleware) or default to history.
      // Only allow same-origin absolute paths: reject "//host" (protocol-
      // relative) and anything not starting with "/" to avoid open redirects.
      const next = new URLSearchParams(window.location.search).get("next");
      const safeNext =
        next && next.startsWith("/") && !next.startsWith("//") ? next : "/history";
      router.push(safeNext);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Login failed.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="wrap">
      <div className="auth-card">
        <div style={{ marginBottom: "1rem" }}>
          <Logo size={30} />
        </div>
        <h1>Welcome back</h1>
        <p className="muted" style={{ marginBottom: "1.5rem" }}>
          Sign in to send mail and see everything you have posted.
        </p>

        <form onSubmit={onSubmit}>
          <label className="field">
            Email
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="email"
            />
          </label>
          <label className="field">
            Password
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          <button className="btn btn-block" type="submit" disabled={busy}>
            {busy ? "Logging in…" : "Log in"}
          </button>
        </form>

        {error && (
          <p className="callout" role="alert" style={{ marginTop: "1rem" }}>
            <IconAlert size={18} />
            <span>{error}</span>
          </p>
        )}

        <div className="auth-alt">
          <p className="muted">
            No account? <Link href="/register">Create one &rarr;</Link>
          </p>
          <p className="muted">
            Or <Link href="/">send as a guest</Link> without one.
          </p>
        </div>
      </div>
    </main>
  );
}
