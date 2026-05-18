import { FormEvent, useState } from "react";

export default function LoginPage() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const onSubmit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      const r = await fetch("/api/auth/login", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      if (r.status === 204) {
        window.location.reload();
        return;
      }
      if (r.status === 401) {
        setError("Invalid username or password.");
      } else {
        setError(`Unexpected response (${r.status}).`);
      }
      setPassword("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Network error");
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-bg px-4">
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm p-6 rounded-z-md bg-bg-elev border border-line shadow-sm"
      >
        <h1 className="font-display text-[20px] text-ink mb-1">Sign in</h1>
        <p className="text-[12px] text-ink-5 mb-5">Zeno is private. Authenticate to continue.</p>

        <div className="mb-3">
          <label
            htmlFor="login-username"
            className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
          >
            Username
          </label>
          <input
            id="login-username"
            type="text"
            autoComplete="username"
            autoFocus
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className="w-full h-9 px-2 rounded-z-sm bg-bg-card border border-line text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            required
          />
        </div>

        <div className="mb-4">
          <label
            htmlFor="login-password"
            className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
          >
            Password
          </label>
          <input
            id="login-password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className="w-full h-9 px-2 rounded-z-sm bg-bg-card border border-line text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            required
          />
        </div>

        {error && (
          <p
            role="alert"
            className="font-mono text-[11px] text-crit mb-3"
          >
            {error}
          </p>
        )}

        <button
          type="submit"
          disabled={pending || username === "" || password === ""}
          className={
            pending || username === "" || password === ""
              ? "w-full h-[32px] rounded-z-sm text-[13px] font-[500] border border-line text-ink-5 opacity-60 cursor-not-allowed"
              : "w-full h-[32px] rounded-z-sm text-[13px] font-[500] bg-accent text-white border border-transparent hover:opacity-90"
          }
        >
          {pending ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
