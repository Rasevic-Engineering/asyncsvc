import { useEffect, useRef } from "react";

// Calls fn every `ms` milliseconds while the tab is visible. Passing null
// pauses polling. The latest fn is always used without restarting the timer.
export function usePolling(fn: () => void, ms: number | null) {
  const saved = useRef(fn);
  saved.current = fn;

  useEffect(() => {
    if (ms === null) return;
    const id = window.setInterval(() => {
      if (document.visibilityState === "visible") saved.current();
    }, ms);
    return () => window.clearInterval(id);
  }, [ms]);
}
