import { initializeApp } from "firebase/app";
import { connectAuthEmulator, getAuth, type Auth } from "firebase/auth";

// Public runtime config served by the Go API at /config.json, so one build
// works in every environment (local emulator, staging, production).
export interface WebConfig {
  firebase: { apiKey: string; authDomain: string; projectId: string };
  authEmulatorUrl?: string;
  googleSignIn: boolean;
}

let loading: Promise<{ auth: Auth; config: WebConfig }> | null = null;

export function loadFirebase() {
  loading ??= (async () => {
    const res = await fetch("/config.json", { cache: "no-store" });
    if (!res.ok) throw new Error(`Couldn't load app config (${res.status})`);
    const config = (await res.json()) as WebConfig;
    const auth = getAuth(initializeApp(config.firebase));
    if (config.authEmulatorUrl) {
      connectAuthEmulator(auth, config.authEmulatorUrl, { disableWarnings: true });
    }
    return { auth, config };
  })().catch((err) => {
    loading = null; // allow a retry
    throw err;
  });
  return loading;
}
