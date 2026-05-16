import clsx from "clsx";
import { useSends, type Send } from "../api/useSends";

// SendsPanel is the V2.13.2 Profile → Sends tab. Lists assistant-mode
// outbound WhatsApp messages from the last 7 days with status badges
// (awaiting_reply / replied / expired). For each replied row, the
// recipient's reply is quoted inline so the user can read the thread
// without opening WhatsApp.
export function SendsPanel() {
  const { data, isLoading, isError } = useSends();

  if (isLoading) {
    return (
      <div className="px-8 py-6 max-w-2xl mx-auto text-ink-4 text-sm">Loading…</div>
    );
  }
  if (isError) {
    return (
      <div className="px-8 py-6 max-w-2xl mx-auto text-rose-400 text-sm">
        Could not load sends.
      </div>
    );
  }

  const sends = data ?? [];

  return (
    <div className="px-8 py-6 max-w-2xl mx-auto space-y-5">
      <header>
        <h2 className="text-ink text-base font-medium">Sends</h2>
        <p className="text-ink-4 text-[13px] mt-1 leading-relaxed">
          Messages Zeno has sent on your behalf in the last 7 days, with the
          recipient's reply when there is one.
        </p>
      </header>

      {sends.length === 0 ? (
        <div className="rounded-md border border-line/60 bg-bg-2 px-4 py-6 text-[13px] text-ink-4">
          No assistant-mode messages yet. When Zeno texts someone for you
          (e.g. confirming a dinner), it will show up here.
        </div>
      ) : (
        <ul className="flex flex-col gap-2.5">
          {sends.map((s) => (
            <SendRow key={s.id} send={s} />
          ))}
        </ul>
      )}
    </div>
  );
}

function SendRow({ send }: { send: Send }) {
  const sent = new Date(send.sent_at);
  return (
    <li className="rounded-md border border-line bg-bg px-4 py-3">
      <div className="flex items-baseline justify-between gap-3 mb-1">
        <span className="text-ink text-[14px] font-medium">
          {send.recipient_name}
        </span>
        <StatusPill status={send.status} />
      </div>
      <div className="font-mono text-[11px] text-ink-4 mb-2">
        {formatSentAt(sent)}
        {send.event_title && (
          <>
            {" · re: "}
            <span className="text-ink-3">{send.event_title}</span>
          </>
        )}
      </div>
      <pre className="whitespace-pre-wrap rounded-z-sm border-l-2 border-ink-5 pl-3 py-1 text-[12.5px] text-ink-2 font-sans m-0">
        {send.draft_body}
      </pre>
      {send.status === "replied" && send.reply_body && (
        <div className="mt-2.5">
          <div className="font-mono text-[10.5px] uppercase tracking-[.06em] text-ink-4 mb-1">
            reply
            {send.resolved_at && (
              <span className="ml-2 normal-case tracking-normal text-ink-4">
                · {formatSentAt(new Date(send.resolved_at))}
              </span>
            )}
          </div>
          <pre className="whitespace-pre-wrap rounded-z-sm border-l-2 border-accent pl-3 py-1 text-[12.5px] text-ink font-sans m-0">
            {send.reply_body}
          </pre>
        </div>
      )}
    </li>
  );
}

function StatusPill({ status }: { status: Send["status"] }) {
  const label =
    status === "replied"
      ? "replied"
      : status === "expired"
        ? "expired"
        : "awaiting reply";
  const tone =
    status === "replied"
      ? "border-accent/40 text-accent bg-accent/5"
      : status === "expired"
        ? "border-line text-ink-4 bg-bg-2"
        : "border-amber/40 text-amber bg-amber-soft";
  return (
    <span
      className={clsx(
        "inline-flex items-center px-2 py-[2px] rounded-full border text-[11px] font-mono uppercase tracking-[.06em]",
        tone
      )}
    >
      {label}
    </span>
  );
}

function formatSentAt(d: Date): string {
  const now = Date.now();
  const diff = now - d.getTime();
  const min = 60_000;
  const hr = 60 * min;
  const day = 24 * hr;
  if (diff < hr) {
    const m = Math.max(1, Math.round(diff / min));
    return `${m}m ago`;
  }
  if (diff < day) {
    const h = Math.round(diff / hr);
    return `${h}h ago`;
  }
  const days = Math.round(diff / day);
  if (days < 7) return `${days}d ago`;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
