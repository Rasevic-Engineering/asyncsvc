import { initializeApp } from "firebase/app";
import { connectAuthEmulator, getAuth, type Auth } from "firebase/auth";
import { connectFirestoreEmulator, getFirestore, type Firestore } from "firebase/firestore";

// Public runtime config served by the Go API at /config.json, so one build
// works in every environment (local emulator, staging, production).
export interface WebConfig {
  firebase: { apiKey: string; authDomain: string; projectId: string };
  authEmulatorUrl?: string;
  firestoreEmulatorHost?: string; // e.g. localhost:8088
  googleSignIn: boolean;
}

let loading: Promise<{ auth: Auth; db: Firestore; config: WebConfig }> | null = null;

export function loadFirebase() {
  loading ??= (async () => {
    const res = await fetch("/config.json", { cache: "no-store" });
    if (!res.ok) throw new Error(`Couldn't load app config (${res.status})`);
    const config = (await res.json()) as WebConfig;
    const app = initializeApp(config.firebase);
    const auth = getAuth(app);
    if (config.authEmulatorUrl) {
      connectAuthEmulator(auth, config.authEmulatorUrl, { disableWarnings: true });
    }
    // Firestore is only used for the admin Activity feed (read-only; access
    // is enforced by Firestore security rules).
    const db = getFirestore(app);
    if (config.firestoreEmulatorHost) {
      const [host, port] = config.firestoreEmulatorHost.split(":");
      connectFirestoreEmulator(db, host, Number(port));
    }
    return { auth, db, config };
  })().catch((err) => {
    loading = null; // allow a retry
    throw err;
  });
  return loading;
}
