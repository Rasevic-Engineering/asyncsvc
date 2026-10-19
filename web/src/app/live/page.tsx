"use client";

import Link from "next/link";
import { Header } from "@/components/Header";
import { LiveData } from "@/components/LiveData";
import { RequireAuth } from "@/components/RequireAuth";
import { useAuth } from "@/lib/auth";

export default function LivePage() {
  return (
    <RequireAuth>
      <Live />
    </RequireAuth>
  );
}

function Live() {
  const { me } = useAuth();
  return (
    <div className="app">
      <Header />
      {me?.admin ? (
        <LiveData />
      ) : (
        <div className="card">
          <h2>Admins only</h2>
          <p className="muted">
            Live data sources are managed by admins. <Link href="/">Back to your reports</Link>
          </p>
        </div>
      )}
    </div>
  );
}
