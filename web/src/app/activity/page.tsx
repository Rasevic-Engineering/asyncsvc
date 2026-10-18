"use client";

import Link from "next/link";
import { ActivityFeed } from "@/components/ActivityFeed";
import { Header } from "@/components/Header";
import { RequireAuth } from "@/components/RequireAuth";
import { useAuth } from "@/lib/auth";

export default function ActivityPage() {
  return (
    <RequireAuth>
      <Activity />
    </RequireAuth>
  );
}

function Activity() {
  const { me } = useAuth();
  return (
    <div className="app">
      <Header />
      {me?.admin ? (
        <ActivityFeed />
      ) : (
        <div className="card">
          <h2>Admins only</h2>
          <p className="muted">
            The activity feed shows every message flowing through the system. <Link href="/">Back to your reports</Link>
          </p>
        </div>
      )}
    </div>
  );
}
