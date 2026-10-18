"use client";

import { useState } from "react";
import { api, type Report } from "@/lib/api";

const LABELS: Record<string, string> = { csv: "CSV", xlsx: "Excel", pdf: "PDF" };
const ORDER = ["csv", "xlsx", "pdf"];

// One button per format the report was generated in. Download links need
// the auth token and expire after a few minutes, so each click asks the API
// for fresh signed links, then navigates to the chosen one.
export function DownloadButtons({ report, onError }: { report: Report; onError: (msg: string) => void }) {
  const [busy, setBusy] = useState<string | null>(null);
  const formats = ORDER.filter((f) => (report.formats ?? ["csv"]).includes(f));

  async function download(format: string) {
    setBusy(format);
    try {
      const r = await api.getReport(report.id);
      const url = r.downloads?.[format] ?? (format === "csv" ? r.download_url : undefined);
      if (!url) throw new Error("This file isn't available for download.");
      window.location.assign(url);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Download failed.");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="downloads" role="group" aria-label="Download report">
      <span className="muted small">Download</span>
      {formats.map((f) => (
        <button key={f} onClick={() => download(f)} disabled={busy !== null} aria-label={`Download ${LABELS[f]}`}>
          {busy === f ? "…" : LABELS[f]}
        </button>
      ))}
    </div>
  );
}
