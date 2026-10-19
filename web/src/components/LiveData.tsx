"use client";

import { useEffect, useMemo, useState } from "react";
import { api, type ReplayMatch } from "@/lib/api";
import { isLive, sortGames, subscribeIngest, type NHLGame, type Replay } from "@/lib/ingest";
import { relativeTime } from "@/lib/format";

const DAY = 24 * 60 * 60 * 1000;

export function LiveData() {
  const [games, setGames] = useState<NHLGame[] | null>(null);
  const [replays, setReplays] = useState<Replay[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [, tick] = useState(0);

  useEffect(
    () =>
      subscribeIngest(
        (g, r) => {
          setGames(g);
          setReplays(r);
          setError(null);
        },
        (err) =>
          setError(
            err.message.includes("permission")
              ? "You don't have access to live data. If you were just made an admin, sign out and back in."
              : err.message,
          ),
      ),
    [],
  );
  useEffect(() => {
    const id = window.setInterval(() => tick((n) => n + 1), 5000);
    return () => window.clearInterval(id);
  }, []);

  // Today's slate: games starting within the last 18h or the next 24h.
  const now = Date.now();
  const todays = useMemo(
    () =>
      sortGames(
        (games ?? []).filter((g) => {
          const t = g.start_time ? Date.parse(g.start_time) : g.updated_at.getTime();
          return t > now - 0.75 * DAY && t < now + DAY;
        }),
      ),
    [games, now],
  );

  return (
    <div className="stack">
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
      <NHLPanel games={todays} loading={games === null && !error} />
      <ReplayPanel replays={replays} />
    </div>
  );
}

function NHLPanel({ games, loading }: { games: NHLGame[]; loading: boolean }) {
  const live = games.filter(isLive).length;
  return (
    <section className="card">
      <div className="card-head">
        <h2>NHL · live play-by-play</h2>
        <span className={`live ${live ? "on" : "paused"}`}>
          <span className="dot" aria-hidden /> {live ? `${live} live` : "No games live"}
        </span>
      </div>
      <p className="muted small">
        The ingester checks the schedule every minute and polls each live game every 10 seconds; new plays
        flow into BigQuery as <code>nhl.*</code> events.
      </p>
      {loading && <p className="muted">Connecting…</p>}
      {!loading && games.length === 0 && (
        <div className="empty">
          <p>No NHL games today.</p>
          <p className="muted">Start a soccer replay below for live data any time.</p>
        </div>
      )}
      {games.length > 0 && (
        <div className="games">
          {games.map((g) => (
            <div key={g.id} className={`game ${isLive(g) ? "game-live" : ""}`}>
              <div className="game-teams">
                <div className="game-row">
                  <span className="team">{g.away}</span>
                  <span className="score">{g.away_score ?? "–"}</span>
                </div>
                <div className="game-row">
                  <span className="team">{g.home}</span>
                  <span className="score">{g.home_score ?? "–"}</span>
                </div>
              </div>
              <div className="game-meta small">
                <GameStatus g={g} />
                {g.away_sog != null && (
                  <span className="muted">
                    Shots {g.away_sog}–{g.home_sog}
                  </span>
                )}
                {g.plays_ingested != null && (
                  <span className="muted">
                    {g.plays_ingested} plays · updated {relativeTime(g.updated_at.toISOString())}
                  </span>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function GameStatus({ g }: { g: NHLGame }) {
  if (isLive(g)) {
    const period = g.period_type === "OT" ? "OT" : g.period_type === "SO" ? "SO" : `P${g.period ?? "?"}`;
    return (
      <span className="badge kind-received">
        <span className="spinner" aria-hidden /> {g.intermission ? `${period} intermission` : `${period} · ${g.clock ?? ""}`}
      </span>
    );
  }
  if (g.state === "FINAL" || g.state === "OFF") return <span className="badge kind-acked">Final</span>;
  const t = g.start_time ? new Date(g.start_time) : null;
  return <span className="badge">{t ? t.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : g.state}</span>;
}

function ReplayPanel({ replays }: { replays: Replay[] }) {
  const [catalog, setCatalog] = useState<ReplayMatch[]>([]);
  const [speeds, setSpeeds] = useState<number[]>([10]);
  const [matchId, setMatchId] = useState<number | null>(null);
  const [speed, setSpeed] = useState(10);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);

  useEffect(() => {
    api.replayCatalog().then(
      (c) => {
        setCatalog(c.matches);
        setSpeeds(c.speeds);
        setMatchId(c.matches[0]?.match_id ?? null);
      },
      (err) => setError(err.message),
    );
  }, []);

  const sorted = useMemo(
    () => [...replays].sort((a, b) => b.updated_at.getTime() - a.updated_at.getTime()).slice(0, 12),
    [replays],
  );
  const running = replays.filter((r) => r.state === "running").length;

  async function start() {
    if (matchId == null) return;
    setBusy(true);
    setError(null);
    try {
      const { replay_id } = await api.startReplay(matchId, speed);
      setPending(replay_id);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn't start the replay.");
    } finally {
      setBusy(false);
    }
  }

  async function stop(id: string) {
    try {
      await api.stopReplay(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn't stop the replay.");
    }
  }

  const minutes = (m: ReplayMatch | undefined) => (m ? Math.round(100 / speed) : 0);
  const selected = catalog.find((m) => m.match_id === matchId);

  return (
    <section className="card">
      <div className="card-head">
        <h2>Soccer match replays</h2>
        {running > 0 && <span className="muted small">{running} running</span>}
      </div>
      <p className="muted small">
        Streams every on-ball event of a historical match (passes, carries, shots with expected goals, fouls)
        in its original timing, sped up, as <code>soccer.*</code> events.
      </p>

      <div className="replay-form">
        <label className="field">
          <span>Match</span>
          <select value={matchId ?? ""} onChange={(e) => setMatchId(Number(e.target.value))} disabled={!catalog.length}>
            {catalog.map((m) => (
              <option key={m.match_id} value={m.match_id}>
                {m.label} — {m.competition}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Speed</span>
          <select value={speed} onChange={(e) => setSpeed(Number(e.target.value))}>
            {speeds.map((s) => (
              <option key={s} value={s}>
                {s === 1 ? "Real time (1×)" : `${s}×`}
              </option>
            ))}
          </select>
        </label>
        <button className="primary" onClick={start} disabled={busy || matchId == null || running >= 3}>
          {busy ? "Starting…" : "Start replay"}
        </button>
      </div>
      {selected && (
        <p className="hint">
          About {minutes(selected)} min at {speed}× · up to 3 replays at a time
        </p>
      )}
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
      {pending && !replays.some((r) => r.replay_id === pending) && (
        <p className="muted small">
          <span className="spinner" aria-hidden /> Waiting for the ingester to pick it up…
        </p>
      )}

      {sorted.length > 0 && (
        <ul className="replays">
          {sorted.map((r) => {
            const pct = r.total ? Math.min(100, Math.round(((r.emitted ?? 0) / r.total) * 100)) : 0;
            const score = r.score ? Object.entries(r.score).map(([t, n]) => `${t} ${n}`).join(" · ") : "";
            return (
              <li key={r.id} className="replay">
                <div className="replay-head">
                  <div className="report-main">
                    <div className="report-title">{r.label ?? `Match ${r.match_id}`}</div>
                    <div className="muted small">
                      {r.competition} · {r.speed}× · {r.requested_by} · {relativeTime(r.updated_at.toISOString())}
                    </div>
                  </div>
                  <span className={`badge ${{ running: "kind-received", finished: "kind-acked", stopped: "", failed: "kind-dead_lettered" }[r.state]}`}>
                    {r.state === "running" && <span className="spinner" aria-hidden />}
                    {r.state[0].toUpperCase() + r.state.slice(1)}
                  </span>
                  {r.state === "running" && (
                    <button onClick={() => stop(r.replay_id)} aria-label={`Stop replay of ${r.label}`}>
                      Stop
                    </button>
                  )}
                </div>
                <div className="progress" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
                  <div className="progress-bar" style={{ width: `${pct}%` }} />
                </div>
                <div className="muted small replay-foot">
                  <span>
                    {r.minute != null ? `${r.minute}'` : ""} {score && <>· {score}</>}
                  </span>
                  <span>
                    {r.emitted ?? 0}/{r.total ?? "…"} events
                  </span>
                </div>
                {r.error && <div className="report-error">{r.error}</div>}
              </li>
            );
          })}
        </ul>
      )}
      <p className="muted small attribution">
        Soccer data: <a href="https://github.com/statsbomb/open-data" target="_blank" rel="noreferrer">StatsBomb open data</a>
        . NHL data: NHL public API.
      </p>
    </section>
  );
}
