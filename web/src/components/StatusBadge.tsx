import type { ReportStatus } from "@/lib/api";

const LABELS: Record<ReportStatus, string> = {
  pending: "Queued",
  running: "Running",
  ready: "Ready",
  failed: "Failed",
};

export function StatusBadge({ status }: { status: ReportStatus }) {
  return (
    <span className={`badge badge-${status}`}>
      {(status === "pending" || status === "running") && <span className="spinner" aria-hidden />}
      {LABELS[status]}
    </span>
  );
}
