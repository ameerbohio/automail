"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";

export default function Nav() {
  const { isAuthenticated, loading, logout } = useAuth();
  const router = useRouter();

  async function onLogout() {
    await logout();
    router.push("/");
  }

  return (
    <nav className="top">
      <Link href="/">Send</Link>
      {isAuthenticated ? (
        <>
          <Link href="/history">Your mail</Link>
          <button className="link" onClick={onLogout}>
            Log out
          </button>
        </>
      ) : (
        <>
          <Link href="/track">Track</Link>
          {!loading && <Link href="/login">Log in</Link>}
        </>
      )}
    </nav>
  );
}
