import clsx from "clsx";
import type {
  SubCard as SubCardData,
  CardAction,
  SubCardAttendee,
  SubCardCalendarDaystrip,
  SubCardCalendarDaystripEvent,
  SubCardCalendarConflict,
  SubCardCalendarAlternative,
  SubCardCalendarRecurring,
  Card as CardData,
} from "../types";
import { useAction } from "../api/useAction";
import { useThreadPreview } from "../api/useThreadPreview";
import { useIntentModes, modeFor } from "../hooks/useIntentModes";
import { renderMarkdownBlocks } from "../lib/markdownBlocks";
import { ActionConfirmModal } from "./ActionConfirmModal";
import { Card as CardComponent } from "./Card";
import { useToast } from "./Toast";
import { useState } from "react";

interface Props {
  cardId: string;
  reply: SubCardData;
}

// SubCard renders one model reply inside a CardFocus thread. Four
// kinds: calendar / draft / research / answer. Action buttons go
// through the V2.8 action surface (useAction) so Send/Confirm/Save
// dispatch identically to Card.tsx actions.
export function SubCard({ cardId, reply }: Props) {
  const { mutateAsync: doAction, isPending } = useAction();
  const { data: intents } = useIntentModes();
  const toast = useToast();
  const [pending, setPending] = useState<{
    action: CardAction;
    preview?: Record<string, unknown>;
  } | null>(null);
  const [hidden, setHidden] = useState(false);
  // V2.13.1: when an action returns a Followup card (e.g. ask_followup
  // from inside the converse modal), render it inline below the
  // sub-card so the user can interact with it. Card.tsx already does
  // this for the morning rail; SubCard previously discarded result.followup
  // which is why an "ambiguous Dana → click candidate → ask_followup
  // → Followup card" flow appeared to do nothing.
  const [followup, setFollowup] = useState<CardData | null>(null);

  function resolveIntent(a: CardAction): string {
    return a.intent && a.intent.length > 0 ? a.intent : a.label.toLowerCase();
  }

  async function dispatch(action: CardAction) {
    const intent = resolveIntent(action);
    const target = (action.target ?? {}) as Record<string, unknown>;

    // Dismiss/snooze on a sub-card means "close this reply", not "dismiss
    // the parent card I'm a follow-up of". The action handler always
    // operates on the parent (sub-cards aren't persisted as their own
    // rows), so dispatching dismiss server-side would silently flip the
    // parent's dismissed flag. Hide locally instead.
    if (intent === "dismiss" || intent === "snooze") {
      setHidden(true);
      return;
    }

    if (intent === "open_url") {
      const url = String(target.url ?? "");
      if (url) window.open(url, "_blank", "noopener,noreferrer");
      doAction({ id: cardId, intent, target }).catch(() => {});
      return;
    }

    const mode = modeFor(intents, intent);
    if (mode === "confirm") {
      setPending({ action });
      return;
    }
    if (mode === "preflight") {
      try {
        const result = await doAction({ id: cardId, intent, target, confirm: false });
        if (!result.ok) {
          toast.push(result.toast || "Could not prepare preview.", "error");
          return;
        }
        if (result.needs_confirm && result.preview) {
          setPending({ action, preview: result.preview });
          return;
        }
        if (result.toast) toast.push(result.toast, "info");
        if (result.followup) setFollowup(result.followup);
      } catch (err) {
        toast.push(`Action failed: ${(err as Error).message}`, "error");
      }
      return;
    }
    try {
      const result = await doAction({ id: cardId, intent, target });
      if (result.toast) toast.push(result.toast, result.ok ? "info" : "error");
      if (result.followup) setFollowup(result.followup);
    } catch (err) {
      toast.push(`Action failed: ${(err as Error).message}`, "error");
    }
  }

  async function commitPending() {
    if (!pending) return;
    try {
      const result = await doAction({
        id: cardId,
        intent: resolveIntent(pending.action),
        target: (pending.action.target ?? {}) as Record<string, unknown>,
        confirm: true,
      });
      if (result.toast) toast.push(result.toast, result.ok ? "info" : "error");
    } catch (err) {
      toast.push(`Action failed: ${(err as Error).message}`, "error");
    } finally {
      setPending(null);
    }
  }

  if (!reply) return null;
  if (hidden) return null;

  return (
    <>
      <div className="rounded-[8px] border border-line bg-bg py-4 px-[18px] animate-sub-in">
        <div className="flex justify-between items-baseline mb-1">
          <span className="font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4">
            {reply.eyebrow}
          </span>
        </div>
        <h5 className="text-[15px] font-[500] leading-[1.4] text-ink m-0 mb-3">
          {reply.title}
        </h5>

        {reply.kind === "calendar" && reply.cal && (
          <SubCalendarBlock cal={reply.cal} conflict={reply.conflict} />
        )}
        {reply.kind === "draft" && (
          <SubDraftBlock body={reply.draft ?? ""} meta={reply.draft_meta} />
        )}
        {reply.kind === "research" && (
          <SubResearchBlock body={reply.body ?? ""} sources={reply.sources} />
        )}
        {reply.kind === "document" && (
          <SubDocumentBlock
            body={reply.body ?? ""}
            from={reply.from}
            threadHint={reply.thread_hint}
          />
        )}
        {reply.kind === "answer" && (
          <p className="text-[14px] leading-[1.6] text-ink m-0">{reply.body}</p>
        )}

        {reply.actions && reply.actions.length > 0 && (
          <div className="mt-3.5 flex flex-wrap items-center gap-1.5">
            {reply.actions.map((a, i) => (
              <button
                key={i}
                type="button"
                disabled={isPending}
                onClick={() => dispatch(a)}
                className={clsx(
                  "h-[26px] px-[9px] rounded-[7px] text-[12px] font-[500] border transition disabled:opacity-50",
                  a.primary
                    ? "bg-ink text-bg border-ink hover:opacity-90"
                    : "border-line text-ink-3 bg-transparent hover:bg-bg-elev hover:border-ink-5",
                )}
              >
                {a.label}
              </button>
            ))}
          </div>
        )}
      </div>

      {followup && (
        <div className="mt-3.5">
          <div className="font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4 mb-2">
            follow-up
          </div>
          <CardComponent card={followup} />
        </div>
      )}

      {pending && (
        <ActionConfirmModal
          intent={resolveIntent(pending.action)}
          preview={pending.preview}
          pending={isPending}
          onConfirm={commitPending}
          onCancel={() => setPending(null)}
        />
      )}
    </>
  );
}

// SubCalendarBlock is the calendar reply body. The legacy minimal grid
// (title/when/where/who) is preserved for old persisted SubCards; when
// the model populates the V2.x rich-detail fields, the block layers in
// the title+duration pill, day-strip mini-timeline, attendee pills with
// status, conflict box, "why this slot" reasoning, alternatives picker,
// and recurring suggestion shown in Zeno V2/zeno-focus.jsx.
function SubCalendarBlock({
  cal,
  conflict,
}: {
  cal: NonNullable<SubCardData["cal"]>;
  conflict?: string;
}) {
  const dur = computeDuration(cal.start, cal.end);
  const hasRich =
    !!dur ||
    !!cal.daystrip ||
    !!cal.attendees?.length ||
    !!cal.conflict ||
    !!cal.reasoning?.length ||
    !!cal.alternatives?.length ||
    !!cal.recurring ||
    !!cal.reminder ||
    (cal.travel_before ?? 0) > 0 ||
    (cal.travel_after ?? 0) > 0;

  return (
    <div className="flex flex-col gap-3.5">
      {hasRich && (
        <div className="flex items-baseline justify-between gap-3">
          <h6 className="text-[14px] font-[500] text-ink m-0">{cal.title}</h6>
          {dur && (
            <span className="font-mono text-[11px] text-ink-3 px-2 py-[2px] rounded-[6px] border border-line">
              {dur}
            </span>
          )}
        </div>
      )}

      {cal.daystrip && cal.daystrip.events && cal.daystrip.events.length > 0 && (
        <DayStrip data={cal.daystrip} />
      )}

      <div className="grid grid-cols-[60px_1fr] gap-x-3.5 gap-y-1.5 text-[13.5px] leading-[1.5]">
        {!hasRich && cal.title && (
          <KVRow k="title" v={cal.title} />
        )}
        <KVRow k="when" v={renderWhen(cal)} />
        <KVRow k="where" v={cal.where} />
        {(cal.travel_before || cal.travel_after) && (
          <KVRow
            k="travel"
            v={renderTravel(cal.travel_before, cal.travel_after)}
            mono
          />
        )}
        {cal.attendees && cal.attendees.length > 0 ? (
          <div className="contents">
            <span className="font-mono text-[11px] uppercase tracking-[.06em] text-ink-4 self-start pt-[2px]">
              who
            </span>
            <div className="flex flex-wrap gap-1.5">
              {cal.attendees.map((a, i) => (
                <AttendeePill key={i} a={a} />
              ))}
            </div>
          </div>
        ) : (
          <KVRow k="who" v={cal.who} />
        )}
        {cal.reminder && <KVRow k="notify" v={cal.reminder} mono />}
      </div>

      {cal.conflict ? (
        <ConflictBox c={cal.conflict} />
      ) : conflict ? (
        <p className="text-[12.5px] text-ink-2 px-3 py-2 bg-bg-elev rounded-[6px] m-0">
          <span className="font-mono text-good">✓</span> {conflict}
        </p>
      ) : null}

      {cal.reasoning && cal.reasoning.length > 0 && (
        <WhyThisSlot reasoning={cal.reasoning} />
      )}

      {cal.alternatives && cal.alternatives.length > 0 && (
        <Alternatives alts={cal.alternatives} />
      )}

      {cal.recurring && <RecurringRow rec={cal.recurring} />}
    </div>
  );
}

function KVRow({ k, v, mono = false }: { k: string; v?: string; mono?: boolean }) {
  if (!v) return null;
  return (
    <div className="contents">
      <span className="font-mono text-[11px] uppercase tracking-[.06em] text-ink-4 self-start pt-[2px]">
        {k}
      </span>
      <span className={clsx("text-ink", mono && "font-mono text-[12.5px] text-ink-2")}>
        {v}
      </span>
    </div>
  );
}

function computeDuration(start?: string, end?: string): string | null {
  if (!start || !end) return null;
  const m = (s: string) => {
    const [h, mm] = s.split(":").map(Number);
    if (Number.isNaN(h) || Number.isNaN(mm)) return null;
    return h * 60 + mm;
  };
  const a = m(start);
  const b = m(end);
  if (a === null || b === null) return null;
  const diff = b - a;
  if (diff <= 0) return null;
  const h = Math.floor(diff / 60);
  const mm = diff % 60;
  if (h && mm) return `${h}h ${mm}m`;
  if (h) return `${h}h`;
  return `${mm}m`;
}

function renderWhen(cal: NonNullable<SubCardData["cal"]>): string | undefined {
  if (cal.when) return cal.when;
  if (cal.start && cal.end) return `${cal.start} → ${cal.end}`;
  return undefined;
}

function renderTravel(before?: number, after?: number): string | undefined {
  const parts: string[] = [];
  if (before && before > 0) parts.push(`+${before} min before`);
  if (after && after > 0) parts.push(`+${after} min after`);
  if (parts.length === 0) return undefined;
  return `${parts.join(" · ")} · auto-blocked`;
}

const ATTENDEE_SYMBOL: Record<string, string> = {
  host: "✦",
  accepted: "●",
  pending: "◌",
  declined: "✕",
};

function AttendeePill({ a }: { a: SubCardAttendee }) {
  const status = a.status || "";
  const sym = ATTENDEE_SYMBOL[status] ?? "·";
  const tone =
    status === "host"
      ? "text-accent"
      : status === "accepted"
        ? "text-good"
        : status === "pending"
          ? "text-amber"
          : status === "declined"
            ? "text-crit"
            : "text-ink-4";
  return (
    <span className="inline-flex items-center gap-1.5 px-2 py-[2px] rounded-[6px] border border-line bg-bg-elev text-[12px] text-ink-2">
      <span className={clsx("font-mono text-[11px]", tone)}>{sym}</span>
      <span>{a.name}</span>
      {a.role && (
        <span className="font-mono text-[10.5px] text-ink-4 lowercase">{a.role}</span>
      )}
    </span>
  );
}

function ConflictBox({ c }: { c: SubCardCalendarConflict }) {
  return (
    <p
      className={clsx(
        "text-[12.5px] m-0 px-3 py-2 rounded-[6px] flex items-start gap-2",
        c.ok ? "bg-good-soft text-ink-2" : "bg-amber-soft text-ink-2",
      )}
    >
      <span className={clsx("font-mono", c.ok ? "text-good" : "text-amber")}>
        {c.ok ? "✓" : "!"}
      </span>
      <span>{c.text}</span>
    </p>
  );
}

function WhyThisSlot({ reasoning }: { reasoning: string[] }) {
  const [open, setOpen] = useState(true);
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((s) => !s)}
        className="flex items-center gap-2 text-[11px] font-mono uppercase tracking-[.06em] text-ink-4 hover:text-ink-3 transition-colors"
      >
        <span aria-hidden>{open ? "▾" : "▸"}</span>
        <span>why this slot</span>
      </button>
      {open && (
        <ul className="mt-2 m-0 pl-4 list-none border-l border-line text-[12.5px] text-ink-2 leading-[1.55]">
          {reasoning.map((r, i) => (
            <li key={i} className="py-[2px]">
              {r}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Alternatives({ alts }: { alts: SubCardCalendarAlternative[] }) {
  const [picked, setPicked] = useState<number | null>(null);
  return (
    <div>
      <span className="block font-mono text-[11px] uppercase tracking-[.06em] text-ink-4 mb-2">
        other windows
      </span>
      <div className="flex flex-col gap-1">
        {alts.map((alt, i) => {
          const on = picked === i;
          return (
            <button
              key={i}
              type="button"
              onClick={() => setPicked(on ? null : i)}
              className={clsx(
                "grid grid-cols-[14px_auto_1fr] items-baseline gap-2 px-2.5 py-1.5 rounded-[6px] border text-left transition-colors",
                on
                  ? "bg-accent-soft border-accent/30"
                  : "border-line hover:bg-bg-elev hover:border-ink-5",
              )}
            >
              <span className={clsx("font-mono text-[11px]", on ? "text-accent" : "text-ink-4")}>
                {on ? "●" : "○"}
              </span>
              <span className="font-mono text-[12px] text-ink-2">{alt.when}</span>
              {alt.note && (
                <span className="text-[12px] text-ink-3 truncate">{alt.note}</span>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}

function RecurringRow({ rec }: { rec: SubCardCalendarRecurring }) {
  const [on, setOn] = useState(rec.default ?? false);
  return (
    <label
      className={clsx(
        "flex items-center gap-2 px-2.5 py-1.5 rounded-[6px] border cursor-pointer text-[12.5px] transition-colors",
        on ? "border-accent/40 bg-accent-soft" : "border-line border-dashed hover:bg-bg-elev",
      )}
    >
      <input
        type="checkbox"
        className="sr-only"
        checked={on}
        onChange={(e) => setOn(e.target.checked)}
      />
      <span
        aria-hidden
        className={clsx(
          "h-[14px] w-[14px] grid place-items-center rounded-[3px] border font-mono text-[10px]",
          on ? "border-accent bg-accent text-bg" : "border-ink-5 text-transparent",
        )}
      >
        ✓
      </span>
      <span className="text-ink-2">{rec.label}</span>
    </label>
  );
}

// DayStrip is the mini timeline that places the proposed slot in the
// context of the day's other events. Positions are clamped to [0,100]
// so an event that overflows the [start_hr, end_hr] window doesn't
// shoot off the strip; an off-strip overflow gets a chevron at the
// near edge so the user knows the proposal extends beyond shown hours.
function DayStrip({ data }: { data: SubCardCalendarDaystrip }) {
  const startHr = data.start_hr ?? 9;
  const endHr = data.end_hr ?? 21;
  const span = Math.max(1, endHr - startHr);
  const events = data.events ?? [];

  const pct = (h: number) => ((h - startHr) / span) * 100;
  const clamp = (n: number) => Math.max(0, Math.min(100, n));

  // Hour ticks every hour, with major emphasis every 3rd hour.
  const ticks: number[] = [];
  for (let h = startHr; h <= endHr; h++) ticks.push(h);

  return (
    <div className="rounded-[6px] border border-line bg-bg-elev/60 overflow-hidden">
      <div className="flex items-baseline justify-between px-2.5 pt-1.5 pb-1 font-mono text-[10.5px] text-ink-4">
        {data.label && <span>{data.label}</span>}
        <span>
          {String(startHr).padStart(2, "0")}:00 → {String(endHr).padStart(2, "0")}:00
        </span>
      </div>
      <div className="relative h-8 mx-2.5 mb-1.5 border-b border-line">
        {ticks.map((h) => {
          const major = (h - startHr) % 3 === 0;
          return (
            <span
              key={h}
              className={clsx(
                "absolute top-0 bottom-0 w-px",
                major ? "bg-line-strong" : "bg-line",
              )}
              style={{ left: `${pct(h)}%` }}
            />
          );
        })}
        {events.map((e, i) => (
          <DayStripEvent key={i} ev={e} pct={pct} clamp={clamp} />
        ))}
      </div>
      <div className="relative h-3 mx-2.5 mb-1.5 font-mono text-[9.5px] text-ink-4">
        {ticks
          .filter((h) => (h - startHr) % 3 === 0)
          .map((h) => (
            <span
              key={h}
              className="absolute -translate-x-1/2"
              style={{ left: `${pct(h)}%` }}
            >
              {String(h).padStart(2, "0")}
            </span>
          ))}
      </div>
    </div>
  );
}

function DayStripEvent({
  ev,
  pct,
  clamp,
}: {
  ev: SubCardCalendarDaystripEvent;
  pct: (h: number) => number;
  clamp: (n: number) => number;
}) {
  const rawLeft = pct(ev.start);
  const rawRight = pct(ev.end);
  const left = clamp(rawLeft);
  const right = clamp(rawRight);
  const width = Math.max(1.5, right - left);
  const overflowLeft = rawLeft < 0;
  const overflowRight = rawRight > 100;

  const kind = ev.kind || "muted";
  const cls =
    kind === "proposed"
      ? "bg-accent text-bg shadow-[0_0_0_2px_var(--accent-soft)]"
      : kind === "travel"
        ? "bg-[repeating-linear-gradient(45deg,var(--ink-5),var(--ink-5)_3px,transparent_3px,transparent_6px)] text-ink-3 opacity-70"
        : "bg-bg-card border border-line text-ink-3";

  return (
    <div
      className={clsx(
        "absolute top-1 bottom-1 rounded-[3px] flex items-center px-1 overflow-hidden",
        cls,
      )}
      style={{ left: `${left}%`, width: `${width}%` }}
      title={ev.label ?? ""}
    >
      {overflowLeft && (
        <span aria-hidden className="font-mono text-[9px] mr-0.5">‹</span>
      )}
      <span className="font-mono text-[9.5px] truncate">{ev.label}</span>
      {overflowRight && (
        <span aria-hidden className="font-mono text-[9px] ml-0.5">›</span>
      )}
    </div>
  );
}

function SubDraftBlock({ body, meta }: { body: string; meta?: string }) {
  return (
    <div>
      <pre className="m-0 px-4 py-3.5 bg-bg-elev rounded-[6px] text-[13.5px] leading-[1.6] text-ink whitespace-pre-wrap break-words border-l-2 border-ink-4 font-sans">
        {body}
      </pre>
      {meta && (
        <div className="mt-2 font-mono text-[10.5px] uppercase tracking-[.04em] text-ink-4 lowercase">
          {meta}
        </div>
      )}
    </div>
  );
}

// SubDocumentBlock renders a `kind=document` reply: lightly-restructured
// markdown of the source content, with a "view original" toggle that
// lazily fetches the verbatim email body from the durable log.
function SubDocumentBlock({
  body,
  from,
  threadHint,
}: {
  body: string;
  from?: string;
  threadHint?: string;
}) {
  const [mode, setMode] = useState<"formatted" | "original">("formatted");
  const canToggle = !!threadHint;
  const preview = useThreadPreview(threadHint, canToggle && mode === "original");

  function toggle() {
    if (!canToggle) return;
    setMode((m) => (m === "formatted" ? "original" : "formatted"));
  }

  return (
    <div>
      {(from || canToggle) && (
        <div className="flex justify-between items-baseline mb-3 gap-3">
          {from ? (
            <span className="font-mono text-[10.5px] uppercase tracking-[.06em] text-ink-4">
              from {from}
            </span>
          ) : (
            <span />
          )}
          {canToggle && (
            <button
              type="button"
              onClick={toggle}
              className="font-mono text-[10.5px] uppercase tracking-[.06em] text-ink-4 hover:text-ink-2 transition-colors"
            >
              {mode === "formatted" ? "view original →" : "← back to formatted"}
            </button>
          )}
        </div>
      )}

      {mode === "formatted" && renderMarkdownBlocks(body)}

      {mode === "original" && (
        <>
          {preview.isLoading && (
            <p className="m-0 text-[12.5px] text-ink-4 italic">loading original…</p>
          )}
          {preview.isError && (
            <p className="m-0 text-[12.5px] text-ink-3">
              Couldn't load the original.
            </p>
          )}
          {preview.data && (
            <pre className="m-0 px-4 py-3.5 bg-bg-elev rounded-[6px] text-[13px] leading-[1.55] text-ink-2 whitespace-pre-wrap break-words border-l-2 border-ink-5 font-sans">
              {preview.data.body}
            </pre>
          )}
        </>
      )}
    </div>
  );
}

function SubResearchBlock({
  body,
  sources,
}: {
  body: string;
  sources?: SubCardData["sources"];
}) {
  return (
    <div>
      <p className="text-[14px] leading-[1.6] text-ink m-0 mb-3.5">{body}</p>
      {sources && sources.length > 0 && (
        <div>
          <div className="font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4 mb-2">
            sources
          </div>
          <ol className="m-0 p-0 list-none flex flex-col gap-1.5">
            {sources.map((s) => (
              <li
                key={s.i}
                className="text-[12.5px] text-ink-2 pl-[22px] relative"
              >
                <span className="absolute left-0 top-[1px] h-4 w-4 rounded-full bg-bg-elev text-ink-3 font-mono text-[10px] grid place-items-center">
                  {s.i}
                </span>
                <b className="text-ink font-[500]">{s.t}</b>
                {s.w && <span className="font-mono text-ink-4"> · {s.w}</span>}
              </li>
            ))}
          </ol>
        </div>
      )}
    </div>
  );
}
