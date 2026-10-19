import { collection, limit, onSnapshot, orderBy, query, where, type Timestamp } from "firebase/firestore";
import { loadFirebase } from "./firebase";

// Written by the ingester to Firestore's "ingest" collection (scoreboards
// and replay progress) and by the worker to "plays" (from the Pub/Sub
// events-to-firestore subscription). See app/internal/ingest, playfeed.

export const SPORTS = [
  { id: "nhl", label: "NHL", icon: "🏒" },
  { id: "mlb", label: "MLB", icon: "⚾" },
  { id: "nba", label: "NBA", icon: "🏀" },
  { id: "nfl", label: "NFL", icon: "🏈" },
  { id: "soccer", label: "Soccer", icon: "⚽" },
] as const;

export type SportId = (typeof SPORTS)[number]["id"];

export interface TeamDoc {
  abbr: string;
  name?: string;
  score?: number;
}

export interface GameDoc {
  id: string;
  kind: "game";
  league: SportId;
  game_id: string;
  state: "scheduled" | "live" | "final";
  status_text?: string;
  start_time?: string;
  home: TeamDoc;
  away: TeamDoc;
  competition?: string;
  extra?: Record<string, number>;
  plays_ingested?: number;
  updated_at: Date;
}

export interface Replay {
  id: string;
  kind: "statsbomb";
  replay_id: string;
  match_id: number;
  label?: string;
  competition?: string;
  speed?: number;
  state: "running" | "finished" | "stopped" | "failed";
  emitted?: number;
  total?: number;
  period?: number;
  minute?: number;
  score?: Record<string, number>;
  requested_by?: string;
  error?: string;
  updated_at: Date;
}

export interface PlayDoc {
  id: string;
  league: string;
  game_id: string;
  type: string;
  text: string;
  team?: string;
  actor?: string;
  period?: string;
  clock?: string;
  matchup?: string;
  competition?: string;
  replay_id?: string;
  scoring: boolean;
  at: Date;
}

const toDate = (v: unknown) => (v as Timestamp | undefined)?.toDate?.() ?? new Date(0);

export function subscribeIngest(
  onChange: (games: GameDoc[], replays: Replay[]) => void,
  onError: (err: Error) => void,
): () => void {
  let unsubscribe = () => {};
  let cancelled = false;
  loadFirebase().then(({ db }) => {
    if (cancelled) return;
    unsubscribe = onSnapshot(
      collection(db, "ingest"),
      (snap) => {
        const games: GameDoc[] = [];
        const replays: Replay[] = [];
        for (const d of snap.docs) {
          const data = d.data();
          const doc = { ...data, id: d.id, updated_at: toDate(data.updated_at) };
          if (data.kind === "game") games.push(doc as GameDoc);
          if (data.kind === "statsbomb") replays.push(doc as Replay);
        }
        onChange(games, replays);
      },
      onError,
    );
  }, onError);
  return () => {
    cancelled = true;
    unsubscribe();
  };
}

// Newest plays of one league ("statsbomb" for replay highlights), live.
export function subscribePlays(
  league: string,
  n: number,
  onChange: (plays: PlayDoc[]) => void,
  onError: (err: Error) => void,
): () => void {
  let unsubscribe = () => {};
  let cancelled = false;
  loadFirebase().then(({ db }) => {
    if (cancelled) return;
    const q = query(collection(db, "plays"), where("league", "==", league), orderBy("at", "desc"), limit(n));
    unsubscribe = onSnapshot(
      q,
      (snap) => onChange(snap.docs.map((d) => ({ ...(d.data() as PlayDoc), id: d.id, at: toDate(d.data().at) }))),
      onError,
    );
  }, onError);
  return () => {
    cancelled = true;
    unsubscribe();
  };
}

const ORDER = { live: 0, scheduled: 1, final: 2 };

// Today's slate for a league: games starting within the last 18h or the
// next 30h, live first.
export function todaysGames(games: GameDoc[], league: string, now = Date.now()): GameDoc[] {
  const day = 24 * 3600 * 1000;
  return games
    .filter((g) => g.league === league)
    .filter((g) => {
      const t = g.start_time ? Date.parse(g.start_time) : g.updated_at.getTime();
      return t > now - 0.75 * day && t < now + 1.25 * day;
    })
    .sort((a, b) => ORDER[a.state] - ORDER[b.state] || (a.start_time ?? "").localeCompare(b.start_time ?? ""));
}
