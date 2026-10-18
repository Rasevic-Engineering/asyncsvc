"use client";

import { sendEmailVerification } from "firebase/auth";
import { useState } from "react";
import { useAuth } from "@/lib/auth";

export function VerifyEmailBanner() {
  const { me, user, refresh } = useAuth();
  const [state, setState] = useState<"idle" | "sent" | "checking">("idle");
  if (!me || me.email_verified || !user?.email) return null;

  return (
    <div className="banner" role="status">
      <span>
        Verify <strong>{user.email}</strong> to get email notifications.
        {state === "sent" && " Check your inbox for the link."}
      </span>
      <span className="banner-actions">
        <button
          className="link"
          onClick={async () => {
            await sendEmailVerification(user).catch(() => {});
            setState("sent");
          }}
        >
          Resend link
        </button>
        <button
          className="link"
          onClick={async () => {
            setState("checking");
            await refresh().catch(() => {});
            setState("idle");
          }}
        >
          {state === "checking" ? "Checking…" : "I've verified it"}
        </button>
      </span>
    </div>
  );
}
