const DAY = 24 * 60 * 60 * 1000;

export const isoDate = (d: Date) => d.toISOString().slice(0, 10);

export const addDays = (date: string, days: number) =>
  isoDate(new Date(Date.parse(date + "T00:00:00Z") + days * DAY));

export const daysBetween = (from: string, to: string) =>
  Math.round((Date.parse(to + "T00:00:00Z") - Date.parse(from + "T00:00:00Z")) / DAY);

export function relativeTime(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  return new Date(iso).toLocaleDateString();
}

export function duration(start?: string, end?: string): string | null {
  if (!start || !end) return null;
  const s = (Date.parse(end) - Date.parse(start)) / 1000;
  return s < 60 ? `${s.toFixed(1)}s` : `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`;
}

export const formatNumber = (n: number) => n.toLocaleString();
