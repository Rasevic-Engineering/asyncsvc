import { collection, onSnapshot, type Timestamp } from "firebase/firestore";
import { loadFirebase } from "./firebase";

// Status documents the ingester writes to Firestore's "ingest" collection
// (see app/internal/ingest).
export interface NHLGame {
  id: string;
  kind: "nhl";
  game_id: number;
  state: string; // FUT, PRE, LIVE, CRIT, FINAL, OFF
  start_time?: string;
  away: string;
  home: string;
  away_score?: number;
  home_score?: number;
  away_sog?: number;
  home_sog?: number;
  period?: number;
  period_type?: string;
  clock?: string;
  intermission?: boolean;
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
  second?: number;
  score?: Record<string, number>;
  requested_by?: string;
  error?: string;
  updated_at: Date;
}

export function subscribeIngest(
  onChange: (games: NHLGame[], replays: Replay[]) => void,
  onError: (err: Error) => void,
): () => void {
  let unsubscribe = () => {};
  let cancelled = false;
  loadFirebase().then(({ db }) => {
    if (cancelled) return;
    unsubscribe = onSnapshot(
      collection(db, "ingest"),
      (snap) => {
        const games: NHLGame[] = [];
        const replays: Replay[] = [];
        for (const d of snap.docs) {
          const data = d.data();
          const doc = { ...data, id: d.id, updated_at: (data.updated_at as Timestamp | undefined)?.toDate() ?? new Date(0) };
          if (data.kind === "nhl") games.push(doc as NHLGame);
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

const STATE_ORDER: Record<string, number> = { LIVE: 0, CRIT: 0, PRE: 1, FUT: 2, FINAL: 3, OFF: 3 };

export function sortGames(games: NHLGame[]): NHLGame[] {
  return [...games].sort(
    (a, b) =>
      (STATE_ORDER[a.state] ?? 9) - (STATE_ORDER[b.state] ?? 9) ||
      (a.start_time ?? "").localeCompare(b.start_time ?? ""),
  );
}

export const isLive = (g: NHLGame) => g.state === "LIVE" || g.state === "CRIT";
