"use client";

import {
  createUserWithEmailAndPassword,
  GoogleAuthProvider,
  onAuthStateChanged,
  sendEmailVerification,
  signInWithEmailAndPassword,
  signInWithPopup,
  signOut as fbSignOut,
  type Auth,
  type User,
} from "firebase/auth";
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { api, setTokenProvider, type Me } from "./api";
import { loadFirebase, type WebConfig } from "./firebase";

type Status = "loading" | "signedOut" | "signedIn" | "error";

interface AuthContextValue {
  status: Status;
  error: string | null;
  user: User | null;
  me: Me | null;
  config: WebConfig | null;
  signIn: (email: string, password: string) => Promise<void>;
  signUp: (email: string, password: string) => Promise<void>;
  signInWithGoogle: () => Promise<void>;
  signOut: () => Promise<void>;
  // Re-reads the user (e.g. after verifying their email) and their roles.
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [auth, setAuth] = useState<Auth | null>(null);
  const [config, setConfig] = useState<WebConfig | null>(null);
  const [status, setStatus] = useState<Status>("loading");
  const [error, setError] = useState<string | null>(null);
  const [user, setUser] = useState<User | null>(null);
  const [me, setMe] = useState<Me | null>(null);

  useEffect(() => {
    let unsubscribe = () => {};
    loadFirebase().then(
      ({ auth, config }) => {
        setAuth(auth);
        setConfig(config);
        // The SDK refreshes the hour-long ID token automatically.
        setTokenProvider(async () => (auth.currentUser ? auth.currentUser.getIdToken() : null));
        unsubscribe = onAuthStateChanged(auth, async (u) => {
          setUser(u);
          if (!u) {
            setMe(null);
            setStatus("signedOut");
            return;
          }
          try {
            setMe(await api.me());
            setStatus("signedIn");
          } catch (err) {
            setError(err instanceof Error ? err.message : "Couldn't load your account");
            setStatus("error");
          }
        });
      },
      (err) => {
        setError(err instanceof Error ? err.message : "Couldn't start sign-in");
        setStatus("error");
      },
    );
    return () => unsubscribe();
  }, []);

  const requireAuth = useCallback(() => {
    if (!auth) throw new Error("Sign-in isn't ready yet");
    return auth;
  }, [auth]);

  const value = useMemo<AuthContextValue>(
    () => ({
      status,
      error,
      user,
      me,
      config,
      signIn: async (email, password) => {
        await signInWithEmailAndPassword(requireAuth(), email, password);
      },
      signUp: async (email, password) => {
        const cred = await createUserWithEmailAndPassword(requireAuth(), email, password);
        await sendEmailVerification(cred.user).catch(() => {});
      },
      signInWithGoogle: async () => {
        await signInWithPopup(requireAuth(), new GoogleAuthProvider());
      },
      signOut: async () => {
        await fbSignOut(requireAuth());
      },
      refresh: async () => {
        const u = requireAuth().currentUser;
        if (!u) return;
        await u.reload();
        await u.getIdToken(true); // picks up email_verified and custom claims
        setMe(await api.me());
      },
    }),
    [status, error, user, me, config, requireAuth],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside <AuthProvider>");
  return ctx;
}

// Human-readable messages for common Firebase Auth errors.
export function authErrorMessage(err: unknown): string {
  const code = (err as { code?: string })?.code ?? "";
  switch (code) {
    case "auth/invalid-credential":
    case "auth/wrong-password":
    case "auth/user-not-found":
      return "Email or password is incorrect.";
    case "auth/email-already-in-use":
      return "An account with this email already exists. Try signing in.";
    case "auth/weak-password":
      return "Use a password with at least 6 characters.";
    case "auth/invalid-email":
      return "Enter a valid email address.";
    case "auth/too-many-requests":
      return "Too many attempts. Wait a moment and try again.";
    case "auth/popup-closed-by-user":
    case "auth/cancelled-popup-request":
      return "";
    case "auth/popup-blocked":
      return "Your browser blocked the sign-in window. Allow pop-ups for this site and try again.";
    case "auth/network-request-failed":
      return "Couldn't reach the sign-in service. Check your connection.";
    default:
      return err instanceof Error ? err.message : "Sign-in failed.";
  }
}
