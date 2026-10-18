"use client";

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, REPORT_KINDS, type Report } from "@/lib/api";
import { addDays, duration, formatNumber, relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { DownloadButtons } from "./DownloadButtons";
import { StatusBadge } from "./StatusBadge";

const kindLabel = (k: string) => REPORT_KINDS.find((x) => x.value === k)?.label ?? k;

// The report a link (e.g. from a "report ready" email) points at.
export function ReportFocus({ id, onClose }: { id: string; onClose: () => void }) {
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setReport(await api.getReport(id));
      setError(null);
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 404
          ? "This report doesn't exist, or it belongs to another account."
          : err instanceof Error
            ? err.message
            : "Couldn't load the report.",
      );
    }
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);
  const active = report?.status === "pending" || report?.status === "running";
  usePolling(load, active ? 2000 : null);

  return (
    <section className="card focus" aria-live="polite">
      <div className="card-head">
        <h2>{report ? kindLabel(report.kind) : "Report"}</h2>
        <button className="link" onClick={onClose}>
          Close
        </button>
      </div>

      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
      {!report && !error && <p className="muted">Loading…</p>}

      {report && (
        <div className="focus-body">
          <div className="report-main">
            <div className="muted small">
              {report.params.from} – {addDays(report.params.to, -1)} · requested {relativeTime(report.created_at)}
              {report.owner_email && <> · by {report.owner_email}</>}
            </div>
            {report.status === "ready" && (
              <div className="muted small">
                {formatNumber(report.row_count ?? 0)} {report.row_count === 1 ? "row" : "rows"}
                {duration(report.started_at, report.completed_at) && (
                  <> · took {duration(report.started_at, report.completed_at)}</>
                )}
              </div>
            )}
            {report.status === "failed" && report.error && <div className="report-error">{report.error}</div>}
          </div>
          <div className="focus-actions">
            <StatusBadge status={report.status} />
            {report.status === "ready" && <DownloadButtons report={report} onError={setError} />}
          </div>
        </div>
      )}
    </section>
  );
}
