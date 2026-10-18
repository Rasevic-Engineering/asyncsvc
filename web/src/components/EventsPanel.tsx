"use client";

import { useState, type FormEvent } from "react";
import { api } from "@/lib/api";

export function EventsPanel() {
  const [eventType, setEventType] = useState("page_view");
  const [userId, setUserId] = useState("user-1");
  const [count, setCount] = useState(10);
  const [props, setProps] = useState('{"path": "/pricing"}');
  const [sending, setSending] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

  let propsError: string | null = null;
  let parsed: Record<string, unknown> | undefined;
  if (props.trim()) {
    try {
      const v = JSON.parse(props);
      if (typeof v !== "object" || v === null || Array.isArray(v)) propsError = "Properties must be a JSON object.";
      else parsed = v;
    } catch {
      propsError = "Properties must be valid JSON.";
    }
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (propsError) return;
    setSending(true);
    setResult(null);
    try {
      const events = Array.from({ length: count }, () => ({
        event_type: eventType.trim(),
        user_id: userId.trim(),
        properties: parsed,
      }));
      const res = await api.sendEvents(events);
      setResult({ ok: true, text: `Sent ${res.accepted} event${res.accepted === 1 ? "" : "s"}.` });
    } catch (err) {
      setResult({ ok: false, text: err instanceof Error ? err.message : "Something went wrong." });
    } finally {
      setSending(false);
    }
  }

  return (
    <form className="card" onSubmit={submit}>
      <div className="card-head">
        <h2>Send test events</h2>
        <span className="badge badge-admin">Admin</span>
      </div>
      <p className="muted">
        Events go to Pub/Sub, which writes them straight into BigQuery. Reports read from there.
      </p>

      <div className="row">
        <label className="field">
          <span>Event type</span>
          <input value={eventType} onChange={(e) => setEventType(e.target.value)} required />
        </label>
        <label className="field">
          <span>User ID</span>
          <input value={userId} onChange={(e) => setUserId(e.target.value)} required />
        </label>
      </div>

      <label className="field">
        <span>How many</span>
        <input
          type="number"
          min={1}
          max={500}
          value={count}
          onChange={(e) => setCount(Math.min(500, Math.max(1, Number(e.target.value) || 1)))}
        />
      </label>

      <label className="field">
        <span>
          Properties <span className="muted">(JSON, optional)</span>
        </span>
        <textarea rows={3} value={props} onChange={(e) => setProps(e.target.value)} spellCheck={false} />
      </label>
      {propsError && <p className="field-error">{propsError}</p>}

      {result && (
        <p className={result.ok ? "success" : "alert"} role="status">
          {result.text}
        </p>
      )}

      <button type="submit" disabled={sending || !!propsError}>
        {sending ? "Sending…" : "Send events"}
      </button>
    </form>
  );
}
