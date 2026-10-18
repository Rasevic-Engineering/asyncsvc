"use client";

import { useCallback, useEffect, useState } from "react";
import { EventsPanel } from "@/components/EventsPanel";
import { Header } from "@/components/Header";
import { ReportForm } from "@/components/ReportForm";
import { ReportFocus } from "@/components/ReportFocus";
import { ReportList } from "@/components/ReportList";
import { RequireAuth } from "@/components/RequireAuth";
import { VerifyEmailBanner } from "@/components/VerifyEmailBanner";
import { api, type Report } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { clearReportParam, reportParam } from "@/lib/nav";
import { usePolling } from "@/lib/usePolling";

export default function DashboardPage() {
  return (
    <RequireAuth>
      <Dashboard />
    </RequireAuth>
  );
}

function Dashboard() {
  const { me } = useAuth();
  const [scope, setScope] = useState<"mine" | "all">("mine");
  const [reports, setReports] = useState<Report[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [highlight, setHighlight] = useState<string | null>(null);
  const [focusId, setFocusId] = useState<string | null>(null);

  // Links from report emails open that report (?report=<id>).
  useEffect(() => {
    const id = reportParam();
    if (id) {
      setFocusId(id);
      setHighlight(id);
    }
  }, []);

  const refresh = useCallback(async () => {
    try {
      setReports(await api.listReports(scope));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "unknown error");
    }
  }, [scope]);

  useEffect(() => {
    setReports(null);
    refresh();
  }, [refresh]);

  // Poll fast while something is in flight, slowly otherwise.
  const active = reports?.some((r) => r.status === "pending" || r.status === "running") ?? false;
  usePolling(refresh, active ? 2000 : 15000);

  function onCreated(r: Report) {
    setReports((prev) => [r, ...(prev ?? []).filter((x) => x.id !== r.id)]);
    setHighlight(r.id);
    window.setTimeout(() => setHighlight((h) => (h === r.id ? null : h)), 2500);
  }

  return (
    <div className="app">
      <Header />
      <VerifyEmailBanner />
      {focusId && (
        <ReportFocus
          id={focusId}
          onClose={() => {
            setFocusId(null);
            setHighlight(null);
            clearReportParam();
          }}
        />
      )}
      <main className="layout">
        <div className="stack">
          <ReportForm onCreated={onCreated} />
          {me?.admin && <EventsPanel />}
        </div>
        <ReportList
          reports={reports}
          error={error}
          highlight={highlight}
          onRetry={refresh}
          scope={scope}
          onScopeChange={me?.admin ? setScope : undefined}
        />
      </main>
    </div>
  );
}
