"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { Logo } from "./icons";

export default function Nav() {
  const { isAuthenticated, loading, logout } = useAuth();
  const router = useRouter();
  const pathname = usePathname();

  async function onLogout() {
    await logout();
    router.push("/");
  }

  // Exact match for "/" so the brand/Send pill isn't lit on every page.
  const isOn = (href: string) =>
    href === "/" ? pathname === "/" : pathname.startsWith(href);

  const linkClass = (href: string) =>
    `nav-link${isOn(href) ? " is-active" : ""}`;

  return (
    <header className="site-header">
      <div className="site-header-inner">
        <Link href="/" className="brand">
          <Logo />
          {/* The wordmark is hidden (not removed) on very narrow screens so
              the nav stays on one line without losing the link's name. */}
          <span className="brand-name">Automail</span>
        </Link>

        <nav className="nav-links" aria-label="Main">
          <Link href="/" className={linkClass("/")}>
            Send
          </Link>
          {isAuthenticated ? (
            <>
              <Link href="/history" className={linkClass("/history")}>
                Your mail
              </Link>
              <button className="link" onClick={onLogout}>
                Log out
              </button>
            </>
          ) : (
            <>
              <Link href="/track" className={linkClass("/track")}>
                Track
              </Link>
              {!loading && (
                <Link href="/login" className={linkClass("/login")}>
                  Log in
                </Link>
              )}
            </>
          )}
        </nav>
      </div>
    </header>
  );
}
