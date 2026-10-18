"use client";

import { useState } from "react";
import { REPORT_KINDS, type Report } from "@/lib/api";
import { addDays, duration, formatNumber, relativeTime } from "@/lib/format";
import { DownloadButtons } from "./DownloadButtons";
import { StatusBadge } from "./StatusBadge";

const kindLabel = (k: string) => REPORT_KINDS.find((x) => x.value === k)?.label ?? k;

interface Props {
  reports: Report[] | null;
  error: string | null;
  highlight: string | null;
  onRetry: () => void;
  scope: "mine" | "all";
  // Only passed for admins.
  onScopeChange?: (s: "mine" | "all") => void;
}

export function ReportList({ reports, error, highlight, onRetry, scope, onScopeChange }: Props) {
  const [downloadError, setDownloadError] = useState<string | null>(null);

  return (
    <section className="card">
      <div className="card-head">
        <h2>{scope === "all" ? "All reports" : "Your reports"}</h2>
        <div className="card-head-right">
          {reports && reports.some((r) => r.status === "pending" || r.status === "running") && (
            <span className="muted small">Updating live</span>
          )}
          {onScopeChange && (
            <div className="segmented" role="group" aria-label="Show reports from">
              <button aria-pressed={scope === "mine"} onClick={() => onScopeChange("mine")}>
                Mine
              </button>
              <button aria-pressed={scope === "all"} onClick={() => onScopeChange("all")}>
                All users
              </button>
            </div>
          )}
        </div>
      </div>

      {downloadError && (
        <p className="alert" role="alert">
          {downloadError}
        </p>
      )}

      {error && (
        <p className="alert" role="alert">
          Couldn't load reports: {error}{" "}
          <button className="link" onClick={onRetry}>
            Try again
          </button>
        </p>
      )}

      {reports === null && !error && <p className="muted">Loading…</p>}

      {reports?.length === 0 && (
        <div className="empty">
          <p>No reports yet.</p>
          <p className="muted">Request one and it'll show up here as it's processed.</p>
        </div>
      )}

      {reports && reports.length > 0 && (
        <ul className="reports">
          {reports.map((r) => (
            <li key={r.id} className={r.id === highlight ? "report flash" : "report"}>
              <div className="report-main">
                <div className="report-title">{kindLabel(r.kind)}</div>
                <div className="muted small">
                  {/* The API's end date is exclusive; show the inclusive one the user picked. */}
                  {r.params.from} – {addDays(r.params.to, -1)} · requested {relativeTime(r.created_at)}
                  {scope === "all" && <> · by {r.owner_email ?? "unknown"}</>}
                  {scope === "mine" && r.notify_email && <> · will email you</>}
                </div>
                {r.status === "failed" && r.error && <div className="report-error">{r.error}</div>}
                {r.status === "pending" && r.attempts > 0 && (
                  <div className="muted small">
                    Retrying (attempt {r.attempts + 1}){r.error ? `: ${r.error}` : ""}
                  </div>
                )}
              </div>
              <div className="report-meta">
                <StatusBadge status={r.status} />
                {r.status === "ready" && (
                  <span className="muted small">
                    {formatNumber(r.row_count ?? 0)} {r.row_count === 1 ? "row" : "rows"}
                    {duration(r.started_at, r.completed_at) && <> · {duration(r.started_at, r.completed_at)}</>}
                  </span>
                )}
              </div>
              <div className="report-action">
                {r.status === "ready" && <DownloadButtons report={r} onError={setDownloadError} />}
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
