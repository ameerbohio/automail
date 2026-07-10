"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

// Sub-navigation shared by the three ops-dashboard pages (plans/07). The
// per-page useAdminData hook handles auth (redirect on 401, "not authorized"
// on 403); this layout is just chrome. Unauthenticated visitors never reach
// here -- the Next middleware redirects /admin/* to /login first.
const tabs = [
  { href: "/admin", label: "Overview" },
  { href: "/admin/jobs", label: "Jobs" },
  { href: "/admin/mailboxes", label: "Mailboxes" },
];

export default function AdminLayout({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  return (
    <div className="wrap">
      <nav className="admin-nav">
        {tabs.map((t) => (
          <Link
            key={t.href}
            href={t.href}
            className={pathname === t.href ? "active" : ""}
          >
            {t.label}
          </Link>
        ))}
      </nav>
      {children}
    </div>
  );
}
