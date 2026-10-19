"use client";

import { useEffect, useMemo, useState } from "react";
import { api, type ReplayMatch } from "@/lib/api";
import {
  SPORTS,
  subscribeIngest,
  subscribePlays,
  todaysGames,
  type GameDoc,
  type PlayDoc,
  type Replay,
  type SportId,
} from "@/lib/ingest";
import { relativeTime } from "@/lib/format";

// Admin Live data page: one pane per sport (scoreboard + live event stream)
// and one for soccer replays. Everything updates in real time from Firestore.
export function LiveData() {
  const [games, setGames] = useState<GameDoc[] | null>(null);
  const [replays, setReplays] = useState<Replay[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [, tick] = useState(0);

  useEffect(
    () =>
      subscribeIngest(
        (g, r) => {
          setGames(g);
          setReplays(r);
          setError(null);
        },
        (err) => setError(friendly(err)),
      ),
    [],
  );
  useEffect(() => {
    const id = window.setInterval(() => tick((n) => n + 1), 10000);
    return () => window.clearInterval(id);
  }, []);

  const now = Date.now();
  const bySport = useMemo(
    () => Object.fromEntries(SPORTS.map((s) => [s.id, todaysGames(games ?? [], s.id, now)])) as Record<SportId, GameDoc[]>,
    [games, now],
  );
  const liveTotal = SPORTS.reduce((n, s) => n + bySport[s.id].filter((g) => g.state === "live").length, 0);

  return (
    <div className="stack">
      <div className="live-summary">
        <span className={`live ${liveTotal ? "on" : "paused"}`}>
          <span className="dot" aria-hidden /> {liveTotal ? `${liveTotal} games live` : "No games live right now"}
        </span>
        <span className="muted small">
          {SPORTS.map((s) => `${s.label} ${bySport[s.id].filter((g) => g.state === "live").length}`).join(" · ")}
        </span>
      </div>
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
      <div className="panes">
        {SPORTS.map((s) => (
          <SportPane
            key={s.id}
            sport={s}
            games={bySport[s.id]}
            loading={games === null && !error}
            expanded={expanded === s.id}
            onExpand={() => setExpanded(expanded === s.id ? null : s.id)}
          />
        ))}
        <ReplayPanel
          replays={replays}
          expanded={expanded === "replays"}
          onExpand={() => setExpanded(expanded === "replays" ? null : "replays")}
        />
      </div>
    </div>
  );
}

function friendly(err: Error) {
  return err.message.includes("permission")
    ? "You don't have access to live data. If you were just made an admin, sign out and back in."
    : err.message;
}

function SportPane({
  sport,
  games,
  loading,
  expanded,
  onExpand,
}: {
  sport: (typeof SPORTS)[number];
  games: GameDoc[];
  loading: boolean;
  expanded: boolean;
  onExpand: () => void;
}) {
  const [filter, setFilter] = useState<string | null>(null);
  const live = games.filter((g) => g.state === "live").length;
  const next = games.find((g) => g.state === "scheduled");

  return (
    <section className={`card pane ${expanded ? "pane-wide" : ""}`} aria-label={`${sport.label} live data`}>
      <div className="card-head">
        <h2>
          {sport.icon} {sport.label}
        </h2>
        <div className="card-head-right">
          <span className={`live ${live ? "on" : "paused"}`}>
            <span className="dot" aria-hidden /> {live ? `${live} live` : "None live"}
          </span>
          <button className="link" onClick={onExpand}>
            {expanded ? "Collapse" : "Expand"}
          </button>
        </div>
      </div>

      {loading && <p className="muted">Connecting…</p>}
      {!loading && games.length === 0 && <p className="muted small">No games today.</p>}
      {games.length > 0 && (
        <div className="chips" role="group" aria-label={`${sport.label} games`}>
          {games.map((g) => {
            const key = `${g.league}:${g.game_id}`;
            const on = filter === key;
            return (
              <button
                key={g.id}
                className={`chip chip-${g.state} ${on ? "chip-on" : ""}`}
                aria-pressed={on}
                onClick={() => setFilter(on ? null : key)}
                title={g.competition ?? undefined}
              >
                <span className="chip-teams">
                  {g.away.abbr} {g.state !== "scheduled" && <b>{g.away.score ?? 0}</b>} @ {g.home.abbr}{" "}
                  {g.state !== "scheduled" && <b>{g.home.score ?? 0}</b>}
                </span>
                <span className="chip-status">
                  {g.state === "scheduled" && g.start_time
                    ? new Date(g.start_time).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
                    : (g.status_text ?? g.state)}
                </span>
              </button>
            );
          })}
        </div>
      )}

      <PlayStream
        league={sport.id}
        filter={filter}
        empty={
          live
            ? "Waiting for the next play…"
            : next?.start_time
              ? `Next: ${next.away.abbr} @ ${next.home.abbr}, ${new Date(next.start_time).toLocaleString([], { weekday: "short", hour: "numeric", minute: "2-digit" })}`
              : "Plays appear here as soon as a game is live."
        }
      />
    </section>
  );
}

// The newest plays of one league, live from Firestore.
function PlayStream({ league, filter, empty }: { league: string; filter: string | null; empty: string }) {
  const [plays, setPlays] = useState<PlayDoc[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(
    () =>
      subscribePlays(
        league,
        60,
        (p) => {
          setPlays(p);
          setError(null);
        },
        (err) => setError(friendly(err)),
      ),
    [league],
  );

  const shown = (plays ?? []).filter((p) => !filter || p.game_id === filter).slice(0, 30);

  if (error) return <p className="alert small">{error}</p>;
  if (plays === null) return <p className="muted small">Connecting…</p>;
  if (shown.length === 0) return <p className="muted small stream-empty">{empty}</p>;
  return (
    <ol className="stream" aria-live="polite">
      {shown.map((p) => (
        <li key={p.id} className={`stream-item ${p.scoring ? "stream-scoring" : ""}`}>
          <span className="stream-meta muted small">
            {[p.period, p.clock].filter(Boolean).join(" · ")}
            {!filter && p.matchup && <span className="stream-game">{p.matchup}</span>}
          </span>
          <span className="stream-text">{p.text || p.type}</span>
          <span className="muted small stream-time" title={p.at.toISOString()}>
            {relativeTime(p.at.toISOString())}
          </span>
        </li>
      ))}
    </ol>
  );
}

function ReplayPanel({ replays, expanded, onExpand }: { replays: Replay[]; expanded: boolean; onExpand: () => void }) {
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
    <section className={`card pane ${expanded ? "pane-wide" : ""}`}>
      <div className="card-head">
        <h2>⏯ Soccer replays</h2>
        <div className="card-head-right">
          {running > 0 && <span className="muted small">{running} running</span>}
          <button className="link" onClick={onExpand}>
            {expanded ? "Collapse" : "Expand"}
          </button>
        </div>
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
      <PlayStream league="statsbomb" filter={null} empty="Start a replay to see its highlights here." />
      <p className="muted small attribution">
        Soccer data: <a href="https://github.com/statsbomb/open-data" target="_blank" rel="noreferrer">StatsBomb open data</a>
        . Live data: NHL API, MLB Stats API, ESPN.
      </p>
    </section>
  );
}
