// Typed client for the Go API. All calls are same-origin and carry the
// signed-in user's Firebase ID token.

export type ReportStatus = "pending" | "running" | "ready" | "failed";

export interface Report {
  id: string;
  kind: string;
  params: { from: string; to: string };
  notify_email?: string;
  status: ReportStatus;
  row_count?: number;
  error?: string;
  attempts: number;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  owner_email?: string;
  // Formats generated for this report (older reports: csv only).
  formats?: string[];
  // Only on GET /v1/reports/{id} once ready: signed links by format.
  downloads?: Record<string, string>;
  // Only on GET /v1/reports/{id} once ready; valid for a few minutes.
  download_url?: string;
}

export interface NewReport {
  kind: string;
  from: string;
  to: string;
  // Email the signed-in user (their verified address) when it's ready.
  notify: boolean;
}

export interface Me {
  uid: string;
  email?: string;
  email_verified: boolean;
  admin: boolean;
}

export interface NewEvent {
  event_type: string;
  user_id: string;
  occurred_at?: string;
  properties?: Record<string, unknown>;
}

export const REPORT_KINDS = [
  { value: "events_by_type", label: "Events by type" },
  { value: "daily_active_users", label: "Daily active users" },
  { value: "raw_events", label: "Raw events" },
] as const;

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

type TokenProvider = () => Promise<string | null>;
let getToken: TokenProvider = async () => null;

// Called by the auth provider once Firebase is ready.
export function setTokenProvider(p: TokenProvider) {
  getToken = p;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const token = await getToken();
  const res = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(token && { Authorization: `Bearer ${token}` }),
      ...init?.headers,
    },
  });
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    throw new ApiError(res.status, body?.error ?? `Request failed (${res.status})`);
  }
  return body as T;
}

export const api = {
  health: () => request<{ status: string }>("/healthz"),

  me: () => request<Me>("/v1/me"),

  listReports: (scope: "mine" | "all" = "mine", limit = 50) =>
    request<{ reports: Report[] }>(`/v1/reports?limit=${limit}&scope=${scope}`).then((r) => r.reports),

  getReport: (id: string) => request<Report>(`/v1/reports/${id}`),

  createReport: (r: NewReport) =>
    request<Report>("/v1/reports", { method: "POST", body: JSON.stringify(r) }),

  sendEvents: (events: NewEvent[]) =>
    request<{ accepted: number; event_ids: string[] }>("/v1/events", {
      method: "POST",
      body: JSON.stringify(events),
    }),
};
