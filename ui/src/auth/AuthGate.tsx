import { useEffect, useState } from "react";
import LoginPage from "./LoginPage";

type Status = "checking" | "authenticated" | "unauthenticated" | "error";

interface Props {
  children: React.ReactNode;
}

export default function AuthGate({ children }: Props) {
  const [status, setStatus] = useState<Status>("checking");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);

  const probe = async () => {
    setStatus("checking");
    setErrorMsg(null);
    try {
      const r = await fetch("/api/auth/me", { credentials: "same-origin" });
      if (r.ok) {
        setStatus("authenticated");
        return;
      }
      if (r.status === 401) {
        setStatus("unauthenticated");
        return;
      }
      setErrorMsg(`Unexpected response (${r.status})`);
      setStatus("error");
    } catch (err) {
      setErrorMsg(err instanceof Error ? err.message : "Network error");
      setStatus("error");
    }
  };

  useEffect(() => {
    void probe();
  }, []);

  if (status === "checking") {
    return (
      <div className="min-h-screen flex items-center justify-center bg-bg text-ink-5 font-mono text-[12px]">
        Loading…
      </div>
    );
  }

  if (status === "unauthenticated") {
    return <LoginPage />;
  }

  if (status === "error") {
    return (
      <div className="min-h-screen flex items-center justify-center bg-bg">
        <div className="max-w-sm w-full p-6 rounded-z-md bg-bg-elev border border-line text-center">
          <p className="font-mono text-[10px] uppercase tracking-wide text-crit mb-2">
            Connection problem
          </p>
          <p className="text-[13px] text-ink mb-4">{errorMsg ?? "Could not reach the server."}</p>
          <button
            onClick={() => {
              void probe();
            }}
            className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] bg-accent text-white border border-transparent hover:opacity-90"
          >
            Retry
          </button>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
