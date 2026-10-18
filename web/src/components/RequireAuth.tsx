"use client";

import { useRouter } from "next/navigation";
import { useEffect, type ReactNode } from "react";
import { useAuth } from "@/lib/auth";
import { currentPath } from "@/lib/nav";

// Client-side gate for the static export. The API enforces auth on every
// request regardless; this only decides what to render.
export function RequireAuth({ children }: { children: ReactNode }) {
  const { status, error } = useAuth();
  const router = useRouter();

  useEffect(() => {
    // Remember where the user was going (e.g. a report link from an email).
    if (status === "signedOut") {
      const here = currentPath();
      router.replace(here === "/" ? "/login" : `/login?next=${encodeURIComponent(here)}`);
    }
  }, [status, router]);

  if (status === "error") {
    return (
      <div className="center">
        <p className="alert" role="alert">
          {error ?? "Something went wrong."}
        </p>
        <button onClick={() => window.location.reload()}>Reload</button>
      </div>
    );
  }
  if (status !== "signedIn") {
    return (
      <div className="center muted" aria-busy>
        <span className="spinner" aria-hidden /> Loading…
      </div>
    );
  }
  return <>{children}</>;
}
