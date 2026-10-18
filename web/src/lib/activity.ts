import { collection, limit, onSnapshot, orderBy, query, type Timestamp } from "firebase/firestore";
import { loadFirebase } from "./firebase";

// One lifecycle event, as written by the worker to Firestore's "activity"
// collection (see app/internal/activity).
export interface ActivityEvent {
  id: string;
  at: Date;
  kind: Kind;
  topic: string;
  service: string;
  subscription?: string;
  message_id?: string;
  outbox_id?: string;
  attempt?: number;
  summary?: string;
  report_id?: string;
  duration_ms?: number;
  error?: string;
}

export type Kind = "published" | "received" | "acked" | "retrying" | "nacked" | "dead_lettered";

// A message and every lifecycle event seen for it, oldest first.
export interface MessageTrail {
  key: string;
  topic: string;
  summary?: string;
  reportId?: string;
  events: ActivityEvent[];
  state: Kind;
  first: Date;
  last: Date;
}

export const FEED_SIZE = 300;

// Subscribes to the newest FEED_SIZE events in real time. Firestore pushes
// changes over its own connection; nothing polls. Returns an unsubscribe.
export function subscribeActivity(
  onEvents: (events: ActivityEvent[]) => void,
  onError: (err: Error) => void,
): () => void {
  let unsubscribe = () => {};
  let cancelled = false;
  loadFirebase().then(({ db }) => {
    if (cancelled) return;
    const q = query(collection(db, "activity"), orderBy("at", "desc"), limit(FEED_SIZE));
    unsubscribe = onSnapshot(
      q,
      (snap) =>
        onEvents(
          snap.docs.map((d) => {
            const data = d.data();
            return { ...data, id: d.id, at: (data.at as Timestamp).toDate() } as ActivityEvent;
          }),
        ),
      onError,
    );
  }, onError);
  return () => {
    cancelled = true;
    unsubscribe();
  };
}

// Groups events into one trail per message: by outbox id (stable across
// publish and delivery), else Pub/Sub message id. Analytics event batches
// have neither and stand alone. Newest activity first.
export function groupByMessage(events: ActivityEvent[]): MessageTrail[] {
  const byKey = new Map<string, MessageTrail>();
  const chronological = [...events].sort((a, b) => a.at.getTime() - b.at.getTime());
  for (const e of chronological) {
    const key = e.outbox_id ?? e.message_id ?? e.id;
    let t = byKey.get(key);
    if (!t) {
      t = { key, topic: e.topic, events: [], state: e.kind, first: e.at, last: e.at };
      byKey.set(key, t);
    }
    t.events.push(e);
    t.state = e.kind;
    t.last = e.at;
    t.summary ??= e.summary;
    t.reportId ??= e.report_id;
  }
  return [...byKey.values()].sort((a, b) => b.last.getTime() - a.last.getTime());
}

export const TOPIC_LABELS: Record<string, string> = {
  "report-requests": "Reports",
  "email-requests": "Emails",
  events: "Events",
};

export const KIND_LABELS: Record<Kind, string> = {
  published: "Published",
  received: "Received",
  acked: "Done",
  retrying: "Retrying",
  nacked: "Failed, will retry",
  dead_lettered: "Dead-lettered",
};

// "Done" for analytics events: Pub/Sub writes them straight to BigQuery,
// so "published" is their final stage in this feed.
export function isFinished(t: MessageTrail): boolean {
  return t.state === "acked" || (t.topic === "events" && t.state === "published");
}
