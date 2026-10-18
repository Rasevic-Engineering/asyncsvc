"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState, type FormEvent } from "react";
import { authErrorMessage, useAuth } from "@/lib/auth";
import { safeNext } from "@/lib/nav";

export default function LoginPage() {
  const { status, config, error: loadError, signIn, signUp, signInWithGoogle } = useAuth();
  const router = useRouter();
  const [mode, setMode] = useState<"signIn" | "signUp">("signIn");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (status === "signedIn") {
      router.replace(safeNext(new URLSearchParams(window.location.search).get("next")));
    }
  }, [status, router]);

  async function run(action: () => Promise<void>) {
    setBusy(true);
    setError(null);
    try {
      await action();
    } catch (err) {
      setError(authErrorMessage(err) || null);
    } finally {
      setBusy(false);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    run(() => (mode === "signIn" ? signIn(email, password) : signUp(email, password)));
  }

  const ready = status === "signedOut";

  return (
    <div className="login">
      <div className="card login-card">
        <div className="brand">
          <img src="/icon.svg" alt="" width={28} height={28} />
          <h1>Async Reports</h1>
        </div>
        <h2>{mode === "signIn" ? "Sign in" : "Create an account"}</h2>

        {status === "error" && (
          <p className="alert" role="alert">
            {loadError}
          </p>
        )}

        {config?.googleSignIn && (
          <>
            <button type="button" disabled={!ready || busy} onClick={() => run(signInWithGoogle)}>
              <GoogleIcon /> Continue with Google
            </button>
            <div className="divider">
              <span>or</span>
            </div>
          </>
        )}

        <form onSubmit={submit} className="stack-tight">
          <label className="field">
            <span>Email</span>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="email"
              required
            />
          </label>
          <label className="field">
            <span>Password</span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={mode === "signIn" ? "current-password" : "new-password"}
              minLength={6}
              required
            />
          </label>
          {error && (
            <p className="alert" role="alert">
              {error}
            </p>
          )}
          <button type="submit" className="primary" disabled={!ready || busy}>
            {busy ? "Please wait…" : mode === "signIn" ? "Sign in" : "Create account"}
          </button>
        </form>

        <p className="muted small center-text">
          {mode === "signIn" ? "New here? " : "Already have an account? "}
          <button
            type="button"
            className="link"
            onClick={() => {
              setMode(mode === "signIn" ? "signUp" : "signIn");
              setError(null);
            }}
          >
            {mode === "signIn" ? "Create an account" : "Sign in"}
          </button>
        </p>
      </div>
    </div>
  );
}

function GoogleIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 48 48" aria-hidden>
      <path fill="#EA4335" d="M24 9.5c3.5 0 6.6 1.2 9.1 3.6l6.8-6.8C35.8 2.4 30.3 0 24 0 14.6 0 6.6 5.4 2.7 13.3l7.9 6.1C12.5 13.6 17.8 9.5 24 9.5z" />
      <path fill="#4285F4" d="M46.1 24.5c0-1.6-.1-3.1-.4-4.5H24v9h12.4c-.5 2.9-2.2 5.3-4.6 6.9l7.4 5.7c4.3-4 6.9-9.9 6.9-17.1z" />
      <path fill="#FBBC05" d="M10.6 28.6c-.5-1.4-.8-3-.8-4.6s.3-3.2.8-4.6l-7.9-6.1C1 16.6 0 20.2 0 24s1 7.4 2.7 10.7l7.9-6.1z" />
      <path fill="#34A853" d="M24 48c6.5 0 11.9-2.1 15.9-5.8l-7.4-5.7c-2.1 1.4-4.8 2.3-8.5 2.3-6.2 0-11.5-4.1-13.4-9.9l-7.9 6.1C6.6 42.6 14.6 48 24 48z" />
    </svg>
  );
}
