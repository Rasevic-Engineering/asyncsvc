"use client";

import { useAuth } from "@/lib/auth";

export function Header() {
  const { me, user, signOut } = useAuth();
  return (
    <header className="header">
      <div className="brand">
        <img src="/icon.svg" alt="" width={28} height={28} />
        <h1>Async Reports</h1>
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
