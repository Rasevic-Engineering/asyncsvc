"use client";

import { useState, type FormEvent } from "react";
import { api, REPORT_KINDS, type Report } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { addDays, daysBetween, isoDate } from "@/lib/format";

const today = isoDate(new Date());

export function ReportForm({ onCreated }: { onCreated: (r: Report) => void }) {
  const { me } = useAuth();
  const canNotify = !!me?.email && me.email_verified;
  const [kind, setKind] = useState<string>(REPORT_KINDS[0].value);
  const [start, setStart] = useState(addDays(today, -6));
  const [end, setEnd] = useState(today);
  const [notify, setNotify] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The API takes a half-open range [from, to); the form shows an inclusive end date.
  const span = daysBetween(start, end) + 1;
  const rangeError =
    span < 1 ? "End date must be on or after the start date." : span > 366 ? "Pick a range of at most 366 days." : null;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (rangeError) return;
    setSubmitting(true);
    setError(null);
    try {
      const report = await api.createReport({
        kind,
        from: start,
        to: addDays(end, 1),
        notify: notify && canNotify,
      });
      onCreated(report);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Something went wrong.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className="card" onSubmit={submit}>
      <h2>Request a report</h2>
      <p className="muted">
        Reports run in the background. You can leave this page; we'll email you when it's ready.
      </p>

      <label className="field">
        <span>Report</span>
        <select value={kind} onChange={(e) => setKind(e.target.value)}>
          {REPORT_KINDS.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </select>
      </label>

      <div className="row">
        <label className="field">
          <span>Start date</span>
          <input type="date" value={start} max={today} onChange={(e) => setStart(e.target.value)} required />
        </label>
        <label className="field">
          <span>End date</span>
          <input type="date" value={end} max={today} onChange={(e) => setEnd(e.target.value)} required />
        </label>
      </div>
      {rangeError ? (
        <p className="field-error">{rangeError}</p>
      ) : (
        <p className="hint">
          {span} day{span === 1 ? "" : "s"}, UTC
        </p>
      )}

      <label className="checkbox">
        <input
          type="checkbox"
          checked={notify && canNotify}
          disabled={!canNotify}
          onChange={(e) => setNotify(e.target.checked)}
        />
        <span>
          Email me when it's ready
          {me?.email && <span className="muted"> ({me.email})</span>}
          {!canNotify && <span className="muted"> · verify your email first</span>}
        </span>
      </label>

      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}

      <button type="submit" className="primary" disabled={submitting || !!rangeError}>
        {submitting ? "Requesting…" : "Request report"}
      </button>
    </form>
  );
}
