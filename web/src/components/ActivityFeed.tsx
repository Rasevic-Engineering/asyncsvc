"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import {
  FEED_SIZE,
  groupByMessage,
  isFinished,
  KIND_LABELS,
  subscribeActivity,
  TOPIC_LABELS,
  type ActivityEvent,
  type MessageTrail,
} from "@/lib/activity";
import { relativeTime } from "@/lib/format";

type StateFilter = "all" | "inflight" | "done" | "problems";

const PROBLEM: ReadonlySet<string> = new Set(["retrying", "nacked", "dead_lettered"]);

function stateOf(t: MessageTrail): StateFilter {
  if (PROBLEM.has(t.state)) return "problems";
  return isFinished(t) ? "done" : "inflight";
}

export function ActivityFeed() {
  const [events, setEvents] = useState<ActivityEvent[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [frozen, setFrozen] = useState<ActivityEvent[] | null>(null);
  const [topic, setTopic] = useState("all");
  const [state, setState] = useState<StateFilter>("all");
  const [open, setOpen] = useState<string | null>(null);
  const [, tick] = useState(0);
  const latest = useRef<ActivityEvent[]>([]);

  useEffect(
    () =>
      subscribeActivity(
        (evs) => {
          latest.current = evs;
          setEvents(evs);
          setError(null);
        },
        (err) =>
          setError(
            err.message.includes("permission")
              ? "You don't have access to the activity feed. If you were just made an admin, sign out and back in."
              : err.message,
          ),
      ),
    [],
  );

  // Keep "12s ago" labels fresh.
  useEffect(() => {
    const id = window.setInterval(() => tick((n) => n + 1), 5000);
    return () => window.clearInterval(id);
  }, []);

  const shown = paused ? frozen : events;
  const trails = useMemo(() => groupByMessage(shown ?? []), [shown]);
  const filtered = trails.filter(
    (t) => (topic === "all" || t.topic === topic) && (state === "all" || stateOf(t) === state),
  );
  const newWhilePaused = paused && frozen ? Math.max(0, (events?.length ?? 0) - frozen.length) : 0;

  const stats = useMemo(() => {
    const done = trails.filter(isFinished);
    const durations = trails.flatMap((t) => t.events.filter((e) => e.kind === "acked" && e.duration_ms != null).map((e) => e.duration_ms!));
    const avg = durations.length ? Math.round(durations.reduce((a, b) => a + b, 0) / durations.length) : null;
    return {
      messages: trails.length,
      inflight: trails.filter((t) => stateOf(t) === "inflight").length,
      done: done.length,
      problems: trails.filter((t) => stateOf(t) === "problems").length,
      avg,
    };
  }, [trails]);

  function togglePause() {
    if (paused) {
      setPaused(false);
      setFrozen(null);
    } else {
      setFrozen(latest.current);
      setPaused(true);
    }
  }

  const topics = ["all", ...Array.from(new Set(trails.map((t) => t.topic))).sort()];

  return (
    <section className="card">
      <div className="card-head">
        <h2>Message activity</h2>
        <div className="card-head-right">
          <span className={`live ${paused ? "paused" : error ? "down" : "on"}`}>
            <span className="dot" aria-hidden /> {paused ? "Paused" : error ? "Disconnected" : "Live"}
          </span>
          <button onClick={togglePause}>
            {paused ? (newWhilePaused > 0 ? `Resume (${newWhilePaused} new)` : "Resume") : "Pause"}
          </button>
        </div>
      </div>

      <div className="stats">
        <Stat label="Messages" value={stats.messages} />
        <Stat label="In flight" value={stats.inflight} />
        <Stat label="Done" value={stats.done} />
        <Stat label="Problems" value={stats.problems} tone={stats.problems ? "warn" : undefined} />
        <Stat label="Avg processing" value={stats.avg == null ? "–" : `${stats.avg} ms`} />
      </div>

      <div className="filters">
        <label>
          <span className="muted small">Topic</span>
          <select value={topic} onChange={(e) => setTopic(e.target.value)}>
            {topics.map((t) => (
              <option key={t} value={t}>
                {t === "all" ? "All topics" : (TOPIC_LABELS[t] ?? t)}
              </option>
            ))}
          </select>
        </label>
        <div className="segmented" role="group" aria-label="Filter by state">
          {(["all", "inflight", "done", "problems"] as StateFilter[]).map((s) => (
            <button key={s} aria-pressed={state === s} onClick={() => setState(s)}>
              {{ all: "All", inflight: "In flight", done: "Done", problems: "Problems" }[s]}
            </button>
          ))}
        </div>
      </div>

      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
      {shown === null && !error && <p className="muted">Connecting…</p>}
      {shown !== null && filtered.length === 0 && (
        <div className="empty">
          <p>No messages{state !== "all" || topic !== "all" ? " match these filters" : " yet"}.</p>
          <p className="muted">Request a report or send test events, and they'll appear here as they're processed.</p>
        </div>
      )}

      {filtered.length > 0 && (
        <ul className="trails">
          {filtered.map((t) => (
            <li key={t.key} className={`trail trail-${stateOf(t)}`}>
              <button className="trail-row" onClick={() => setOpen(open === t.key ? null : t.key)} aria-expanded={open === t.key}>
                <span className="topic-badge">{TOPIC_LABELS[t.topic] ?? t.topic}</span>
                <span className="trail-summary">{t.summary ?? "message"}</span>
                <Stages trail={t} />
                <span className={`badge kind-${isFinished(t) ? "acked" : t.state}`}>
                  {isFinished(t) ? "Done" : KIND_LABELS[t.state]}
                </span>
                <span className="muted small trail-time">{relativeTime(t.last.toISOString())}</span>
              </button>
              {open === t.key && <TrailDetail trail={t} />}
            </li>
          ))}
        </ul>
      )}
      <p className="muted small">
        Showing the newest {FEED_SIZE} events (kept for 7 days; full history is in BigQuery{" "}
        <code>analytics.message_activity</code>).
      </p>
    </section>
  );
}

function Stat({ label, value, tone }: { label: string; value: number | string; tone?: "warn" }) {
  return (
    <div className={`stat ${tone ?? ""}`}>
      <div className="stat-value">{value}</div>
      <div className="muted small">{label}</div>
    </div>
  );
}

// Compact pipeline: one dot per stage, coloured by outcome.
function Stages({ trail }: { trail: MessageTrail }) {
  return (
    <span className="stages" aria-label={trail.events.map((e) => KIND_LABELS[e.kind]).join(", then ")}>
      {trail.events.map((e) => (
        <span key={e.id} className={`stage kind-${e.kind}`} title={`${KIND_LABELS[e.kind]}${e.attempt ? ` (attempt ${e.attempt})` : ""}`} />
      ))}
    </span>
  );
}

function TrailDetail({ trail }: { trail: MessageTrail }) {
  const t0 = trail.first.getTime();
  return (
    <div className="trail-detail">
      <table>
        <thead>
          <tr>
            <th>Stage</th>
            <th>When</th>
            <th>Attempt</th>
            <th>Took</th>
            <th>By</th>
            <th>Details</th>
          </tr>
        </thead>
        <tbody>
          {trail.events.map((e) => (
            <tr key={e.id}>
              <td>
                <span className={`badge kind-${e.kind}`}>{KIND_LABELS[e.kind]}</span>
              </td>
              <td title={e.at.toISOString()}>{e.at.getTime() === t0 ? e.at.toLocaleTimeString() : `+${((e.at.getTime() - t0) / 1000).toFixed(2)}s`}</td>
              <td>{e.attempt ?? "–"}</td>
              <td>{e.duration_ms != null ? `${e.duration_ms} ms` : "–"}</td>
              <td>{e.service}</td>
              <td className="detail-cell">{e.error ?? e.subscription?.split("/").pop() ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="muted small">
        {trail.events[0].message_id && <>Pub/Sub message {trail.events.find((e) => e.message_id)?.message_id} · </>}
        {trail.events[0].outbox_id && <>outbox {trail.events[0].outbox_id} · </>}
        {trail.reportId && (
          <a href={`/?report=${trail.reportId}`}>Open report</a>
        )}
      </p>
    </div>
  );
}
