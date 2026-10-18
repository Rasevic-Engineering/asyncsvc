// Where to send the user after signing in. Only same-site paths are allowed,
// so a crafted ?next= link can't bounce someone to another website.
export function safeNext(next: string | null | undefined): string {
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.includes("\\")) return "/";
  return next;
}

export function currentPath(): string {
  return window.location.pathname + window.location.search;
}

// Reads ?report=<id> (links in report emails).
export function reportParam(): string | null {
  return new URLSearchParams(window.location.search).get("report");
}

export function clearReportParam() {
  const url = new URL(window.location.href);
  url.searchParams.delete("report");
  window.history.replaceState(null, "", url.pathname + url.search);
}
