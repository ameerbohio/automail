"use client";

import { useState, type FormEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { Logo, IconAlert } from "../icons";

const MIN_PASSWORD = 8;

export default function RegisterPage() {
  const { register } = useAuth();
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (password.length < MIN_PASSWORD) {
      setError(`Password must be at least ${MIN_PASSWORD} characters.`);
      return;
    }
    setBusy(true);
    try {
      // Registration auto-logs-in, so we land straight in the account flow.
      await register(email, password);
      router.push("/history");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Registration failed.");
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
        <h1>Create an account</h1>
        <p className="muted" style={{ marginBottom: "1.5rem" }}>
          An account keeps your history in one place, so you never have to save
          a per-job token. Sending as a guest never requires one.
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
              autoComplete="new-password"
            />
          </label>
          <p className="muted" style={{ margin: "-0.5rem 0 1rem", fontSize: "0.8125rem" }}>
            At least {MIN_PASSWORD} characters.
          </p>
          <button className="btn btn-block" type="submit" disabled={busy}>
            {busy ? "Creating…" : "Create account"}
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
            Already have an account? <Link href="/login">Log in &rarr;</Link>
          </p>
        </div>
      </div>
    </main>
  );
}
