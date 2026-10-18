"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useAuth } from "@/lib/auth";

export function Header() {
  const { me, user, signOut } = useAuth();
  const path = usePathname();
  return (
    <header className="header">
      <div className="brand">
        <img src="/icon.svg" alt="" width={28} height={28} />
        <h1>Async Reports</h1>
        {me?.admin && (
          <nav className="nav" aria-label="Main">
            <Link href="/" aria-current={path === "/" ? "page" : undefined}>
              Reports
            </Link>
            <Link href="/activity" aria-current={path?.startsWith("/activity") ? "page" : undefined}>
              Activity
            </Link>
          </nav>
        )}
      </div>
      {user && (
        <div className="account">
          <span className="account-email" title={me?.email ?? user.email ?? ""}>
            {me?.email ?? user.email}
          </span>
          {me?.admin && <span className="badge badge-admin">Admin</span>}
          <button onClick={() => signOut()}>Sign out</button>
        </div>
      )}
    </header>
  );
}
